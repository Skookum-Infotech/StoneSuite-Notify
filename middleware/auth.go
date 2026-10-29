// Package middleware provides HTTP middleware shared across handlers:
// JWT session auth (compatible with StoneSuite-Backend tokens) and
// internal service-to-service auth for the notification-create endpoint.
package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"stonesuite-notify/models"
)

type contextKey string

const userContextKey contextKey = "userContext"

// UserContext holds the authenticated caller's identity, decoded from the
// same JWT claim shape StoneSuite-Backend issues at login (id, email,
// tenant_id, user_id, active_role_id).
//
// Permissions is read from an optional "permissions" claim. StoneSuite-
// Backend does not mint it today, so it is usually empty — see Can in
// permissions.go for how authorization degrades to the self-service set in
// that case.
//
// AccessibleResources is read from an optional "accessible_resources" claim
// and names the notification `resource` values (e.g. "estimate",
// "salesorder") the caller may see. An empty list means unrestricted — it is
// not yet minted by StoneSuite-Backend for every role, so absence must not
// hide notifications from callers who predate the claim.
type UserContext struct {
	ID                  string
	Email               string
	TenantID            string
	UserID              string
	ActiveRoleID        string
	Permissions         []string
	AccessibleResources []string
}

// RequireAuth verifies the incoming JWT (Bearer header or the httpOnly
// auth_token cookie, mirroring StoneSuite-Backend's own auth middleware so
// the same browser session works against both services) and injects the
// caller's identity into the request context.
func RequireAuth(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			tokenString, ok := extractToken(r)
			if !ok {
				deny(w, http.StatusUnauthorized, "Access denied. No authorization token provided.")
				return
			}

			token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, errors.New("unexpected signing method")
				}
				return []byte(jwtSecret), nil
			}, jwt.WithValidMethods([]string{"HS256"}))
			if err != nil || !token.Valid {
				message := "Authentication failed. Invalid or malformed token."
				if errors.Is(err, jwt.ErrTokenExpired) {
					message = "Authentication session expired. Please sign in again."
				}
				deny(w, http.StatusUnauthorized, message)
				return
			}

			claims, ok := token.Claims.(jwt.MapClaims)
			if !ok {
				deny(w, http.StatusUnauthorized, "Authentication failed. Failed to parse claims.")
				return
			}

			identityID, okID := claims["id"].(string)
			email, okEmail := claims["email"].(string)
			tenantID, _ := claims["tenant_id"].(string)
			activeRoleID, _ := claims["active_role_id"].(string)

			if !okID || !okEmail || tenantID == "" {
				deny(w, http.StatusUnauthorized, "Authentication failed. Invalid token claims.")
				return
			}

			// StoneSuite-Backend's tokens carry the caller's identity only in
			// "id" — there is no separate "user_id" claim (generateTenantJWT
			// never sets one), so "id" doubles as the per-user scoping key.
			ctx := contextWithUser(r.Context(), UserContext{
				ID:                  identityID,
				Email:               email,
				TenantID:            tenantID,
				UserID:              identityID,
				ActiveRoleID:        activeRoleID,
				Permissions:         parseStringListClaim(claims["permissions"]),
				AccessibleResources: parseStringListClaim(claims["accessible_resources"]),
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractToken pulls the JWT from the Authorization header first, falling
// back to the auth_token cookie set by StoneSuite-Backend.
func extractToken(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		parts := strings.Split(authHeader, " ")
		if len(parts) == 2 && parts[0] == "Bearer" {
			return parts[1], true
		}
		return "", false
	}
	if cookie, err := r.Cookie("auth_token"); err == nil && cookie.Value != "" {
		return cookie.Value, true
	}
	return "", false
}

// contextWithUser attaches an authenticated identity to a request context.
// It is the only writer of userContextKey, so GetUserFromContext can rely
// on the value's type.
func contextWithUser(ctx context.Context, user UserContext) context.Context {
	return context.WithValue(ctx, userContextKey, user)
}

// GetUserFromContext extracts the authenticated UserContext from a request
// context populated by RequireAuth.
func GetUserFromContext(ctx context.Context) (UserContext, error) {
	val := ctx.Value(userContextKey)
	if val == nil {
		return UserContext{}, errors.New("no user context found in request")
	}
	payload, ok := val.(UserContext)
	if !ok {
		return UserContext{}, errors.New("invalid user context payload type")
	}
	return payload, nil
}

// RequireInternalSecret gates service-to-service endpoints (notification
// creation triggered by a business event in the main backend) behind a
// shared secret header, since there is no end-user session on that call path.
func RequireInternalSecret(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Secret")), []byte(secret)) != 1 {
				deny(w, http.StatusUnauthorized, "Access denied. Invalid internal service credentials.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func deny(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(models.APIResponse{Success: false, Message: msg})
}

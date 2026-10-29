// Command signwebhook prints the Svix headers for a webhook body, so a
// developer can POST a hand-written event to a local
// /api/webhooks/resend without involving Resend. Local testing only.
//
// Usage:
//
//	go run ./scripts/signwebhook -secret whsec_... -body '{"type":"email.bounced",...}'
//	go run ./scripts/signwebhook -secret "$RESEND_WEBHOOK_SECRET" -id msg_demo_1 -body "$(cat event.json)"
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"stonesuite-notify/emailevents"
)

func main() {
	secret := flag.String("secret", os.Getenv("RESEND_WEBHOOK_SECRET"), "signing secret (whsec_…); defaults to $RESEND_WEBHOOK_SECRET")
	id := flag.String("id", "msg_local_"+strconv.FormatInt(time.Now().UnixNano(), 36), "svix-id (must be unique per event)")
	body := flag.String("body", "", "exact request body to sign")
	flag.Parse()

	if *secret == "" || *body == "" {
		fmt.Fprintln(os.Stderr, "signwebhook: -secret (or $RESEND_WEBHOOK_SECRET) and -body are required")
		os.Exit(1)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature, err := emailevents.SignatureHeader(*secret, *id, timestamp, []byte(*body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "signwebhook: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("svix-id: %s\nsvix-timestamp: %s\nsvix-signature: %s\n", *id, timestamp, signature)
}

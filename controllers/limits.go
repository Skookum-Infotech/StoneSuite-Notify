package controllers

const (
	// maxUserBodyBytes caps JSON bodies on end-user routes (push, preferences).
	maxUserBodyBytes = 64 << 10
	// maxCreateBodyBytes caps the internal create call: up to 25 MB of
	// attachments decoded is ~34 MB as base64, plus the HTML body.
	maxCreateBodyBytes = 40 << 20
	// maxRecipients bounds one create call's fan-out.
	maxRecipients = 500
)

package listener

// Metric names emitted by the listener. Like loopcmd's, they are constants so
// a test asserts on the spelling the code sends.
//
// None carries an issue or pull request number. The repo tag is bounded by
// the repositories this machine routes; the event and stage tags are fixed
// vocabularies.
const (
	mWebhookReceived  = "webhook.received"
	mWebhookRejected  = "webhook.rejected"
	mWebhookDeduped   = "webhook.deduped"
	mWebhookDropped   = "webhook.dropped"
	mWebhookUnrouted  = "webhook.unrouted"
	mPollPassDuration = "poll.pass.duration_ms"
)

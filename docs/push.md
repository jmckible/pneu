# Push sync: Gmail watch over Pub/Sub (not built)

A sketch to return to. Today the server polls every 30 seconds
(`gmi.Options.Interval`), plus a sync on open and focus. A phone hears of
new mail within seconds through Gmail's own push; pneu hears of it on the
next poll. The goal: the bar's count moves when the watch buzzes, so the
message is already there when the window opens.

## Shape

- **Gmail side.** `users.watch` per account, `topicName` our topic,
  `labelIds: ["INBOX"]`, `labelFilterBehavior: "include"`. Gmail then
  publishes `{emailAddress, historyId}` on each change to INBOX. A watch
  expires after 7 days: renew daily, and on startup.
- **Topic.** In the GCP project that already holds the OAuth client lieer
  uses. `gmail-api-push@system.gserviceaccount.com` gets
  `roles/pubsub.publisher` on the topic.
- **Delivery is a pull subscription.** A push subscription needs a public
  HTTPS endpoint; the server is on the tailnet and stays there. The server
  long-polls `POST https://pubsub.googleapis.com/v1/{subscription}:pull`
  outbound and acks what it got. `net/http` and `crypto/rsa` (a service
  account's RS256 JWT for its token) are enough: no Google SDK, stdlib
  rule intact.
- **A message is a nudge, nothing more.** It maps `emailAddress` to an
  account (unknown: ignore, or sync all) and requests a sync, the same
  queued `OpSync` the poll and `/sync` use. `historyId` is never read:
  lieer owns history, and nothing from Google's payload reaches a command,
  a page or status.json. The worst a forged or replayed message can do is
  cause a sync the poll would have run anyway.
- **Polling stays** as the fallback (Google says notifications can be late
  or dropped), relaxed to a few minutes once push is healthy. The engine
  doesn't change: push is one more caller of the sync request.
- **Server only.** A client gets the result as it does today, through the
  link's `sync`/`view`/`status` events.

## Credentials (the decision to make first)

PLAN.md says pneu never talks to Google; this would change that, and needs
two credentials lieer doesn't give us.

- **Subscriber:** a service account with `roles/pubsub.subscriber` on the
  one subscription. All it can learn is "something changed at history N".
  Key file in the state dir, 0600, checked on load like `peer/server.pem`.
- **`users.watch`** needs a Gmail-scoped token per account. Options:
  1. **pneu's own token, scope `gmail.metadata`**, through one more consent
     per account (`pneu account watch <name>`, reusing the auth relay so it
     works from a client). The narrowest grant. **Preferred.**
  2. Read lieer's token. Its scopes cover `watch`, and refreshing an access
     token doesn't rewrite lieer's file, so there's no race. But it breaks
     "credentials are only stat'ed" (onboarding), and a pneu bug then
     holds a token that can send mail.
  3. A service account with domain-wide delegation. Workspace domains you
     administer only, and far more power than this needs.

  One user OAuth token carrying both `gmail.metadata` and `pubsub` would
  avoid the service account, but the pubsub scope reaches every project
  the user can touch. Keep them split.

## Gotchas

- **Our own writes notify us.** Archive or trash changes INBOX, so pneu's
  debounced push produces a notification, which asks for a sync that finds
  nothing. Harmless, but notifications must coalesce: one queued sync per
  account at most (the engine's queue already does this), and a floor on
  spacing so a burst of triage isn't a burst of `gmi sync`.
- **Watch failure has to be visible.** An expired watch or a revoked
  metadata token silently degrades to polling. Put a watch state per
  account in the sync health status.json and the page already show,
  without letting it turn the bar red while polling still works.
- **Pull loop and sleep.** The long-poll connection dies on suspend; on
  `internal/wake`'s wall-clock jump, drop it, re-pull, and run one sync,
  since notifications may have expired while we were down.
- **Ack after the sync is queued**, not after it completes: redelivery
  would only cause a second sync, and acking late keeps a flood of
  redeliveries out of a slow sync.
- **Setup is the cost.** Topic, IAM binding, subscription, service
  account, key, and one more consent per account. That belongs in
  INSTALL.md as an optional section (unrehearsed, like the client path),
  with the server running fine without it.

## Rejected

- **IMAP IDLE.** Push over one outbound connection, no GCP, about 150
  lines by hand. But it needs `https://mail.google.com/` or an app
  password: full mailbox access to learn "something changed".
- **Pub/Sub push subscription** (Google POSTs to us): needs a public
  endpoint (Tailscale Funnel at best), for nothing pull doesn't give.

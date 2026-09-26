# Pneu

A Gmail-native mail client for Omarchy. Named for the Paris *pneumatique*
(1866–1984), where a letter went by tube under the city and arrived within the
hour; Parisians called the letter itself *un pneu*.

Gmail stays the backend. Pneu is a local view over a synced copy: lieer syncs
each account via the Gmail API, notmuch indexes each into its own database, and
a Go server on localhost renders them in an `--app` window of your browser. An
Omarchy bar widget shows unread mail and warns when sync stops. The app never
touches the network — lieer is the modem, notmuch is the disk.

See [PLAN.md](PLAN.md) for scope and build order.

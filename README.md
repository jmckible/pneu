<p align="center"><picture><source media="(prefers-color-scheme: dark)" srcset="brand/stamp-inked-dark.webp"><img src="brand/stamp-inked.webp" width="400" alt="The pneu postmark: a p drawn as a pneumatic tube, ringed by “dans l'heure · within the hour”"></picture></p>

<h1 align="center"><picture><source media="(prefers-color-scheme: dark)" srcset="brand/wordmark-paper.svg"><img src="brand/wordmark-ink.svg" height="72" alt="pneu"></picture></h1>

A Gmail-native mail client for Omarchy. Named for the Paris *pneumatique*
(1866–1984), where a letter went by tube under the city and arrived within the
hour; Parisians called the letter itself *un pneu*.

Gmail stays the backend. pneu is a local view over a synced copy: lieer syncs
each account via the Gmail API, notmuch indexes each into its own database, and
a Go server on localhost renders them in an `--app` window of your browser. An
Omarchy bar widget shows unread mail and warns when sync stops. The app never
touches the network — lieer is the modem, notmuch is the disk.

![pneu's inbox beside an open thread, with fictional mail](docs/screenshot.webp)

See [PLAN.md](PLAN.md) for scope and build order.

The brand, its reasoning and its assets are in [brand/](brand/README.md).

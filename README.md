# Sonarr & Radarr Autoscan for Spark

An Autoscan source for Spark Server and upstream
[Silo](https://github.com/Silo-Server/silo-server). On each poll, it reads a
Sonarr or Radarr server's history since the previous poll and reports files
imported, renamed or deleted in between, so the server scans only those folders.

It needs no webhook, so it also works when Sonarr or Radarr cannot reach the
server. Part of the Spark community plugins.

## Install

Add the plugin from the Spark community catalog, or add this repository index
under **Admin → Plugins → Repositories**:

    https://raw.githubusercontent.com/Joloxx9/spark-plugin-autoscan-arr/spark/index.json

## Set up

1. Open **Admin → Libraries → Autoscan** and enable Autoscan.
2. Choose **Add source → Sonarr & Radarr history**.
3. Pick or add a Sonarr or Radarr connection (URL and API key).
4. If Sonarr or Radarr sees your media under different paths than the server, add a
   path rewrite on the source.

Add one source per Sonarr or Radarr server.

## What it reports

| History event | Reported |
|---|---|
| Download or folder imported | the imported file |
| File renamed | the new file and the old one, so the old entry is marked missing |
| File deleted | the deleted file |

When an event carries no usable file path, the series or movie folder is
scanned instead. The first poll starts at the newest history record, so
nothing older is replayed. A long backlog is read over several polls, at most
2,000 events at a time.

## Development

    make check    # tests, vet, gofmt, module verification, tool tests
    make dist     # release archives for every declared platform

Releases are published by pushing a `v*` tag; the workflow builds the
binaries, publishes the release and updates `index.json` on `spark`.

## License

AGPL-3.0-or-later. Written from scratch for Spark from the public Silo plugin
SDK (Apache-2.0) and the Sonarr and Radarr APIs.

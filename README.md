# Conversion API

A shared Go service for conversion events from multiple apps. It accepts confirmed
events, stores them in a durable file outbox, and forwards them to the X App Conversion
API. The service runs independently of your apps and has no external Go dependencies.

`config/apps.example.json` provides an application configuration template. Supported
events are `INSTALL`, `PURCHASE`, and `TUTORIAL_COMPLETE`. The working configuration
`config/apps.json`, secrets in `.env`, and event data in `data/` are excluded from Git
and the Docker build context.

## Scope and responsibilities

This service delivers confirmed conversions. It does not determine the source of an
installation, validate Google Play purchase tokens, or verify tutorial completion.

`POST /v1/apps/{key}/events` is intended for trusted server-side producers, such as
attribution handlers, purchase validators, or progress handlers. An ingestion secret
authorizes submissions for one specific app. A mobile client's `confirmed: true`
assertion is not verification. Never embed `EXAMPLE_INGEST_TOKEN` or an X token in an
APK/IPA. Direct mobile submissions require a separate endpoint with client verification
(for example, Play Integrity), rate limiting, and server-side purchase validation.
That endpoint is not implemented yet.

Mobile app integration is a separate step. To use the X integration, deploy this
service, configure its token, and connect a source of confirmed events. An HTTP 200
response from X confirms receipt; it does not guarantee attribution to a particular ad.

## Local setup

Requires Go 1.21+ on macOS/Linux. Tested with Go 1.21.13. Use a supported Go release
for deployment; the Dockerfile uses 1.27.3. On macOS with Go 1.21, the Makefile supplies
the SDK path, uses the system linker, and applies local ad hoc signatures to the
binaries. This compatibility setup requires the Xcode command line tools.

```sh
cp .env.example .env
cp config/apps.example.json config/apps.json
# Set your app_id and x_account_id in config/apps.json.
openssl rand -hex 32
# Save the result as EXAMPLE_INGEST_TOKEN in .env.
# Add X_APP_CONVERSION_TOKEN after generating it in X Ads Manager.

make test
make build
./bin/conversion-api
```

The server listens on `127.0.0.1:8080` by default and stores its outbox in `./data`.
Both commands, `conversion-api` and `send-test`, automatically read `.env` from the
current working directory. Existing environment variables take precedence, including
explicitly empty values. Comments, the `export` prefix, and single- and double-quoted
values are supported. Shell code and variable substitution are not evaluated; values
are treated literally. The default file may be absent when the environment is supplied
by a shell, process manager, or Docker Compose.

Server flags: `-listen`, `-data`, `-config`, and `-env-file`. Select another file with
`-env-file /path/to/.env`, or disable file loading with `-env-file ""`. An explicitly
selected file that does not exist returns an error, except for the optional default
`.env`. Configuration and data paths are also relative to the current working directory.

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

`healthz` checks that the process responds. `readyz` returns 503 until every configured
app has an X token. Without an X token, the service stores events but does not send
them. An empty ingestion secret, or one shared between apps, prevents startup.

## Adding apps

Add an entry to `config/apps.json` with a unique `key`, a unique `os_type`/`app_id`
pair, and its own `ingest_token_env` variable. `key` identifies the API route, while
`app_id` is the package identifier sent to X. Clients cannot override the app ID,
platform, or X account in a request.

```json
{
  "key": "another-android",
  "app_id": "com.example.another",
  "os_type": "ANDROID",
  "x_account_id": "YOUR_X_AD_ACCOUNT_ID",
  "x_token_env": "ANOTHER_X_APP_CONVERSION_TOKEN",
  "ingest_token_env": "ANOTHER_INGEST_TOKEN",
  "events": ["INSTALL", "PURCHASE", "TUTORIAL_COMPLETE"]
}
```

Secrets are supplied through the server environment; the JSON file stores only their
variable names. Multiple apps may reference the same X token variable if that token
has the required permissions. Restart the process after changing its configuration
or environment.

For hosted deployments without a configuration file, set `CONVERSION_APPS_JSON` to
the complete JSON object with an `apps` array, using the same format as
`config/apps.example.json`. A nonempty value takes precedence over `-config` in both
commands. An invalid value fails startup instead of falling back to a file. An unset
or whitespace-only value preserves file-based configuration. The JSON contains app
settings and environment variable names; keep the actual tokens in separate variables.

## Submitting events

```http
POST /v1/apps/example-android/events
Authorization: Bearer <EXAMPLE_INGEST_TOKEN>
Content-Type: application/json
```

```json
{
  "events": [
    {
      "event_id": "install:verified-installation-id",
      "event_name": "INSTALL",
      "conversion_timestamp": 1792576800000,
      "identifiers": {
        "device_id": "11111111-2222-4333-8444-555555555555"
      },
      "limit_ad_tracking": false
    }
  ]
}
```

The UUID and timestamp above illustrate the format; they are not a ready-to-send
test event. Use the confirmed event's actual time in **Unix milliseconds**, not the
time of a retry. The API accepts 1–500 events per request, each with at least one
valid identifier. New event types must be explicitly added to the code and allowlist.

Keep `event_id` stable for each action: use an installation ID for INSTALL, the
server-side ID of a verified order for PURCHASE, and an installation ID plus tutorial
version for TUTORIAL_COMPLETE. After a network error, retry the original request with
the same IDs and content. A different payload with an existing ID returns 409. Retrying
the original batch is safe if only part of it was saved. App updates, purchase restores,
and reopening the tutorial completion screen must not create new events.

A 202 response means the event was saved to the outbox. Check delivery separately:

```http
GET /v1/apps/example-android/events/install:verified-installation-id
Authorization: Bearer <EXAMPLE_INGEST_TOKEN>
```

Example of a successful delivery status:

```json
{
  "app_key": "example-android",
  "event_id": "install:verified-installation-id",
  "state": "sent",
  "attempts": 1,
  "updated_at": "2026-10-28T00:00:00Z",
  "x_http_status": 200,
  "debug_id": "debug-id-from-x"
}
```

### Identifiers

`identifiers` is an object containing one or more fields:

| Fields | Processing |
|---|---|
| `device_id` | A real, nonzero advertising UUID; sent without hashing |
| `email` | Trim → lowercase → SHA-256 hex |
| `phone_number` | Remove spaces, parentheses, and hyphens → validate E.164 with an explicit `+` and country code → SHA-256 hex |
| `hashed_email`, `hashed_phone_number` | Existing SHA-256 hex digests; not hashed again |
| `ip_address` together with `user_agent` | Both are required for this pair; use the original user's address and user agent |

Phone numbers without a country code are rejected; the service does not guess the
country. Do not supply both raw and hashed versions of the same contact field.
Raw email addresses and phone numbers are not written to the outbox. Request bodies
and tokens are not logged. An intermediary server's IP address and user agent are
not suitable for user matching. The API does not insert them automatically or trust
arbitrary `X-Forwarded-For` headers.

Android producers must explicitly supply `limit_ad_tracking` based on device settings
or data received from the device. An advertising ID is rejected when that flag is set.
Only submit data you are permitted to use for advertising measurement.

For `IOS` configurations, `att_status` is required instead of the Android flag. The
flag sent to X is derived from ATT. This endpoint supports identifiable iOS conversions
only when the status is `authorized`; `denied`, `restricted`, and `not_determined` are
rejected. Aggregate SKAdNetwork attribution is a separate flow and is not implemented here.

### Purchases and tutorial completion

`PURCHASE` additionally requires verified value, currency, and product information:

```json
{
  "event_id": "purchase:verified-order-id",
  "event_name": "PURCHASE",
  "conversion_timestamp": 1792576800000,
  "identifiers": {"hashed_email": "<64-character-sha256-hex>"},
  "limit_ad_tracking": false,
  "value": 7.99,
  "price_currency": "USD",
  "number_items": 1,
  "contents": [
    {"content_id": "example.full_game", "content_type": "product", "num_items": 1}
  ]
}
```

Submit the event after the server has verified the purchase token and purchase state.
The price above is only an example; use the order's actual currency and value. Debug
unlocks, promotional codes, pending/cancelled/failed purchases, and entitlement restores
are not new PURCHASE events.

Submit tutorial completion as `TUTORIAL_COMPLETE` with the same base fields. Define a
specific completion criterion in your app; displaying a screen or closing a hint does
not itself establish completion. A trusted producer must verify this transition.
Create Purchase and Tutorial complete events for the app in X Events Manager;
X creates only Install when the integration is connected.

## Delivery and operations

The outbox is grouped by app, with up to 500 conversions per POST to
`https://ads-api.x.com/12/accounts/{account}/measurement/app_conversions`.
The `X-App-Conversion-Token` header is read from the corresponding environment variable.
HTTP redirects are not followed. Success requires HTTP 200 **and**
`data.conversions_processed` equal to the batch size. The `debug_id` is saved.

| State | Meaning |
|---|---|
| `pending` | Waiting for delivery or a retry after 401/403/429; honors Retry-After |
| `sending` | Delivery has started; the state is saved before the HTTP request |
| `sent` | X acknowledged the entire batch; identifiers have been removed from the record |
| `failed` | X rejected the event (400/404/422); no automatic retry |
| `uncertain` | Timeout, network failure, 5xx, incomplete response, or restart during delivery; the outcome is unknown |

The supplied X API contract has no idempotency key. Our `event_id` prevents duplicate
outbox submissions but cannot guarantee exactly-once processing inside X. Events in
the `uncertain` state are not retried automatically. Check the event in X first;
an explicit retry is available through:

```http
POST /v1/apps/example-android/events/install:verified-installation-id/retry
Authorization: Bearer <EXAMPLE_INGEST_TOKEN>
Content-Type: application/json

{"accept_duplicate_risk":true}
```

Only `failed` and `uncertain` events can be retried. Sent records remain as deduplication
metadata; these IDs are not deleted automatically. Unsent records contain normalized
identifiers. Restrict access to the data directory and its backups.

This version uses one process per data directory. A file lock prevents two dispatchers
from using the same directory. Storage must be persistent and local, with support for
`fsync` and atomic rename. Do not scale replicas over shared NFS. Scaling requires a
shared database with transactional job claiming. Memory usage and the number of files
grow with the number of unique events; this initial version is intended for a small
shared service.

### Docker and persistent storage

For Coolify, use [`compose.coolify.yaml`](compose.coolify.yaml) and the instructions
below. The default `compose.yaml` in this section is for self-managed deployments
with an explicitly created external volume.

The container writes its outbox and deduplication records to `/data`. The Dockerfile
declares this directory as a `VOLUME`; Compose mounts the external named volume
`conversion-api-data`. Its name is independent of the Compose project name or
deployment directory.

For the first deployment, create and populate `.env` and `config/apps.json` from the
example files, then run the following from the project root. The working configuration
is mounted read-only and is not embedded in the image:

```sh
docker volume create conversion-api-data
docker compose up --build -d
```

`docker volume create` creates the volume once; repeating it with the same name does
not clear existing data. To rebuild the image and recreate the container:

```sh
docker compose up --build -d --force-recreate
```

The same volume is mounted again, preserving the outbox, delivery statuses, and
deduplication records. The volume is marked `external: true`, so its lifecycle is
independent of Compose: neither `docker compose down` nor `docker compose down -v`
removes it. The files remain on the Docker host. Moving to another host requires
migrating the volume or restoring a backup. Manually deleting the volume deletes
these records.

Select an existing volume with `CONVERSION_DATA_VOLUME` in `.env`. Compose requires
that the specified external volume already exist. If you previously used the older
Compose configuration with a `<project>_conversion-data` volume, specify its exact
name to reuse the existing data without migration.

To run without Compose:

```sh
docker build -t conversion-api:local .
docker volume create conversion-api-data
docker run -d --name conversion-api --restart unless-stopped \
  --env-file .env \
  --mount type=bind,source="$PWD/config/apps.json",target=/config/apps.json,readonly \
  --mount type=volume,source=conversion-api-data,target=/data \
  -p 127.0.0.1:8080:8080 \
  conversion-api:local
```

For a new named volume, Docker preserves the ownership of the prepared `/data`
directory, allowing the service to run as the unprivileged UID/GID `65532:65532`.
For a bind mount (`/srv/path:/data`) or an existing volume, ensure that this UID can
write to the directory. Do not use anonymous volumes for redeployment: a new container
does not automatically locate an earlier anonymous volume.

The local `./data` directory used outside Docker is not copied into the volume
automatically. To migrate existing data, stop the local service first, copy the
contents of `./data/` into the volume with ownership `65532:65532`, then start the
container.

Compose publishes the port only on localhost. For external access, configure an HTTPS
reverse proxy and restrict access to trusted producers. Use one container per volume;
the outbox lock prevents concurrent delivery by multiple instances.

### Coolify deployment

Use `compose.coolify.yaml` for a Git-based Docker Compose application in Coolify.
It requires no host configuration file and no manually created Docker volume.

1. Set **Docker Compose Location** to `/compose.coolify.yaml`, save, and reload the
   Compose definition from the repository. Keep the same Coolify resource for redeploys.
2. In **Environment Variables**, set `CONVERSION_APPS_JSON` to the complete contents
   of your private `config/apps.json`. JSON can be entered on one line. Supply every
   token variable referenced by its `ingest_token_env` and `x_token_env` fields as
   runtime environment variables. For the example configuration these are
   `EXAMPLE_INGEST_TOKEN` (a unique random secret of at least 32 characters) and
   `X_APP_CONVERSION_TOKEN`. Custom token variable names also work through `env_file`.
   Tokens are needed only at runtime; disable build-time availability for them.
3. Review **Persistent Storage** after reloading. This definition mounts only a
   managed volume at `/data`. It does not mount `/config/apps.json`; the previous
   directory/file mount at that path is not needed. Do not copy private app settings
   into the public repository to provide that file.
4. If using a public domain, point Coolify's proxy at the service's internal port,
   for example `https://conversions.example.com:8080`. This file exposes port 8080
   inside the Docker network rather than publishing a host port.
5. Deploy and check `/healthz`, `/readyz`, and runtime logs. Without an X token,
   `/readyz` returns 503 while events remain queued.

Coolify creates the managed volume on the first deployment and reuses it when the
same resource is redeployed. Its deployed name may include the resource UUID; check
the rendered Compose definition for the exact name. Recreating the resource is not
the same as redeploying it: migrate the volume if moving data to a different resource
or server. Explicit volume deletion (including Compose `down -v` on this managed
stack) can remove the data. This differs from the external-volume setup above.

If deployment fails with `external volume "conversion-api-data" not found`, it is
using the self-managed `compose.yaml`, which deliberately requires a pre-created
volume. Select `compose.coolify.yaml` for automatic provisioning, or create the
external volume on the deployment server when intentionally using the self-managed
setup. A mount shown under **Directories** with destination `/config/apps.json` is
also incorrect: that path expects a file. The Coolify definition avoids that mount
by loading `CONVERSION_APPS_JSON` directly.

## Testing with a real X token

1. In X Ads Manager, open **Send events to X App Conversion API** and click
   **Generate access token**. Save the token as `X_APP_CONVERSION_TOKEN` on the server.
2. Restart the service and check `/readyz`.
3. Set `X_TEST_DEVICE_ID` in `.env` to the raw advertising ID of a real Android test
   device with advertising measurement allowed, then run from the project root:

```sh
./bin/send-test
# For a deployed service:
./bin/send-test -url https://conversions.example.com
```

The command submits a **real INSTALL**, not a sandbox event, and waits for `state=sent`
with `x_http_status=200`. It prints `conversions_processed=1` and `debug_id` on success.
Each invocation creates a new test event, so do not run it automatically. Then verify
receipt in X Events Manager. Live delivery cannot be verified without an X token.

The project's tests use a local HTTP mock and never send events to X.

## wherenow

Self-hosted backend for [Where Now?](https://apps.apple.com/se/app/where-now/id6757414541?l=en-GB), an iOS app for tracking where you've been.

Stores location data as JSONL on a mounted volume. Deployed to [fly.io](https://fly.io).

### API

All endpoints except `GET /?ping=1` require a `Bearer` token in the `Authorization` header.

**Ping**

```
GET /?ping=1          # unauthenticated
GET /?ping=auth       # authenticated
```

**Save location**

```
POST /
{"id":"...","lat":59.33,"lon":18.07,"timestamp":"2026-01-01T12:00:00Z","accuracy":12,"label":"Stockholm","note":"...","category":"Travel","reason":"upload"}
```

**Read locations**

```
GET /                 # returns up to 200 most recent upload entries
GET /?limit=50        # custom limit (max 200)
```

**Patch metadata**

```
PATCH /
{"id":"...","label":"New label","note":"Updated note","category":"Updated category"}
```

**Delete location**

```
DELETE /
{"id":"..."}
```

### Deploy

Requires `flyctl` and a `FLY_API_TOKEN` secret in the GitHub repository for CI.

```
fly secrets set TOKEN=<your-bearer-token>
fly deploy
```

Pushes to `master` trigger automatic deployment via GitHub Actions.

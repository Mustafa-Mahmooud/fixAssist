# Free deployment: Northflank + Postgres

A $0 setup with no card that stays on all the time. Northflank's free Developer Sandbox runs the app (the only always-on free container host found in November 2026; Render's free tier sleeps, and Vercel only runs short functions). The database is Northflank's free Postgres addon or Supabase's free Postgres. Free plans change: check [northflank.com/pricing](https://northflank.com/pricing) and [supabase.com/pricing](https://supabase.com/pricing).

- **What runs:** one container from the repository's `Dockerfile`. `app serve` runs ingest, the dashboard and the background workers, applies migrations on start, and listens on `PORT` (or 8787).
- **Country database:** baked into the image when it's built.
- **Address:** Northflank gives a permanent HTTPS address like `https://<service>--<id>.code.run`.

## 1. Accounts (the founder)

1. Sign up at [northflank.com](https://northflank.com) with GitHub, and choose the free **Developer Sandbox**.
2. Allow Northflank's GitHub app to read the `fixAssist` repository.

## 2. Database

**Option A: Northflank's Postgres addon** (simplest; everything in one place)

1. In the project, go to **Add new > Addon > PostgreSQL**. Pick the newest version and the smallest free size.
2. Open the addon's **Connection details** and copy the connection URI for internal (private) use.

**Option B: Supabase free Postgres** (if the Sandbox has no free addon slot left)

1. At [supabase.com](https://supabase.com), create a project and note its database password.
2. Under **Connect > Session pooler**, copy the URI (port 5432). Use the session pooler, not the transaction pooler (port 6543), because the job queue needs a real session. Add `?sslmode=require` at the end.

The app is always using the database, so Supabase's "pause after a week without activity" doesn't apply.

## 3. The service

In the project: **Add new > Service > Combined service** (it builds and runs).

| Setting | Value |
| --- | --- |
| Repository / branch | `fixAssist` / `main` |
| Build type | Dockerfile, path `/Dockerfile`, context `/` |
| Port | `8787`, HTTP, **public** (this is the address you'll use) |
| Health check | HTTP `GET /healthz` on 8787 |
| Instances / plan | 1 × the smallest free plan |

**Runtime variables** (Northflank > service > Environment; mark the secrets as secret):

| Variable | Value |
| --- | --- |
| `DATABASE_URL` | the connection URI from step 2 |
| `APP_SECRET` | 32+ random characters (signs feedback links and GitHub state) |
| `APP_PUBLIC_URL` | the service's public URL, `https://….code.run` |
| `INGEST_PUBLIC_URL` | the same URL |
| `TRUST_PROXY` | `1` (behind Northflank's load balancer) |
| `STAFF_EMAILS` | your email (opens `/admin`) |
| `SMTP_HOST` / `SMTP_PORT` | `smtp.gmail.com` / `587` |
| `SMTP_USERNAME` | the Gmail address |
| `SMTP_PASSWORD` | the Gmail app password (secret) |
| `EMAIL_FROM` | `Fix Assistant <the Gmail address>` |

Deploy. The first build takes a few minutes. When it's live:

1. Open `https://…code.run/healthz`. It should say `ok`.
2. Open `https://…code.run/login`, enter your email, and click the link that arrives by email.
3. Create the project, then install the snippet from **Setup**. The address never changes now.

## Updating

Every push to `main` makes Northflank rebuild and redeploy (auto-deploy is on by default). That also refreshes the country database. Migrations run on start.

## Limits to keep in mind

- **The free machine is small.** That's fine for a handful of sites. Moving to a paid plan is a setting, not a rebuild.
- **Database storage:** Supabase free gives 500 MB. Events are kept for 30 days and old daily partitions are dropped automatically. Admin > Ingest shows the volume.
- **Backups:** these free databases don't include point-in-time backups. Export from the provider's dashboard if the data matters.

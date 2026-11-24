# Deploying Fix Assistant (free tier)

One VM runs three containers from `deploy/compose.yml`:
- **Caddy**: HTTPS with automatic Let's Encrypt certificates;
- **app**: the Go binary, with the ingest, web and worker roles;
- **Postgres 16** with pgvector, plus WAL-G backups to Cloudflare R2.

Pushing to `main` runs the CI tests, builds the x86 image into GHCR and deploys over SSH.

| Piece | Choice | Cost |
|---|---|---|
| Server | Google Cloud e2-micro (always free): Ubuntu 24.04, 2 shared vCPU / 1 GB RAM + 2 GB swap, 30 GB disk, US region | $0 (card needed; 1 GB/month of outbound traffic is free) |
| Hostname | DuckDNS, e.g. `fixassist.duckdns.org` | $0 |
| Email | off: partners get sign-in links from `admin login-link` | $0 |
| Backups | Cloudflare R2: continuous WAL plus a nightly full backup, 7 kept, monthly restore test | $0 up to 10 GB |
| CI/CD | GitHub Actions + GHCR (private repo) | $0 within free minutes |

Email needs a domain we control (DKIM/SPF records), so it stays off until we buy one. Then set `RESEND_API_KEY`, `EMAIL_FROM` and `EMAIL_MODE=` in `.env` and redeploy.

## Your steps

### 1. Google Cloud VM (e2-micro, always free)
1. Go to <https://console.cloud.google.com>, accept the terms and create a project (e.g. `fixassist`). Under **Billing**, add a card. You aren't charged while you stay inside the free tier.
2. Go to **Compute Engine → VM instances → Create instance** (enable the Compute Engine API when asked):
   - **Name**: `fixassist`.
   - **Region**: `us-east1`, `us-central1` or `us-west1`. Only these three are free.
   - **Machine type**: `e2-micro`.
   - **Boot disk**: Ubuntu 24.04 LTS (x86/64), *Standard persistent disk*, 30 GB.
   - **Firewall**: tick *Allow HTTP traffic* and *Allow HTTPS traffic*.
   - **Security → Manage access → Add item**: paste the SSH key line ending in `ubuntu`. The word at the end becomes the login user.
3. Go to **VPC network → IP addresses** and promote the VM's external IP to **static**, so it never changes. A static IP is free while attached to a running VM.
4. Copy the VM's **external IP**.

### 2. DuckDNS hostname
Sign in at <https://www.duckdns.org> (GitHub or Google), add a subdomain (e.g. `fixassist`) and set its **current ip** to the VM's IP.

### 3. Cloudflare R2 bucket
1. Go to <https://dash.cloudflare.com>, open **R2**, and enable it. It may ask for a payment method; the first 10 GB are free.
2. Create a bucket, e.g. `fixassist-backups`.
3. Go to **Manage R2 API tokens → Create API token**, with **Object Read & Write** permission limited to that bucket. Copy the **Access Key ID**, the **Secret Access Key** and the **S3 endpoint** (`https://<account-id>.r2.cloudflarestorage.com`).

### 4. GitHub repository
1. Create a **private** repository, e.g. `fixassist`, with no README.
2. In **Settings → Environments**, create `production`.
3. In **Settings → Secrets and variables → Actions**, add these repository secrets:
   - `DEPLOY_HOST`: the VM's public IP
   - `DEPLOY_USER`: `ubuntu`
   - `DEPLOY_SSH_KEY`: the whole contents of `C:\Users\Win11\.ssh\fixassist_ci` (the private key file, without `.pub`)

### 5. Hand over
Fill in `deploy/.env.production` on this PC: server IP, DuckDNS name, R2 values and repo name. It is git-ignored. Then tell Claude it's done.

## What Claude does next
1. Renames the Go module to `github.com/<you>/<repo>`, makes the first commit and pushes to `main` (Git Credential Manager opens a browser login the first time).
2. Over SSH: copies `deploy/`, runs `sudo bootstrap.sh` (Docker, firewall, `/opt/fixassist`, cron), writes `.env` from your values, and authorizes the CI key.
3. The first CI run tests, builds and deploys. Then it checks `https://<domain>/healthz` and the TLS certificate.
4. Runs a first backup and a restore test.
5. Runs the spike app against the public URL, creates your sign-in link, and walks through the dashboard.

## After the first deploy

1. **Uptime check (free):** create an HTTP(s) monitor at <https://uptimerobot.com> (or Better Stack) for `https://<domain>/healthz` every 5 minutes, alerting your email. `/healthz` also pings Postgres, so it catches a stopped database too.
2. **Staff access:** put your email in `STAFF_EMAILS` in `/opt/fixassist/.env` and run `./deploy.sh` to open `/admin`.
3. **Backups:** run `./backup.sh` once by hand, then `./restore-test.sh`. The restore test must print `restore test ok` before you invite partners.
4. **Metrics** (on the server): `docker compose exec app wget -qO- --header "Authorization: Bearer $METRICS_TOKEN" http://localhost:8787/metrics`, or set `METRICS_TOKEN` and scrape from inside the Docker network. Caddy never serves `/metrics` publicly.

## Day-to-day
```sh
ssh -i ~/.ssh/fixassist_admin ubuntu@<ip>
cd /opt/fixassist
docker compose ps
docker compose logs -f app
docker compose exec app /app admin login-link --email partner@example.com   # sign-in link for a partner (valid 3 days)
docker compose exec app /app admin list-projects
./backup.sh            # also runs nightly at 03:30 UTC
./restore-test.sh      # also runs monthly
```

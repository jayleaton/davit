# CI runners: GitHub Actions jobs in fresh boxes

A davit host can run your GitHub Actions jobs. For each queued job that asks for it, the host registers a
single-use runner, starts a fresh box from the runner image, runs the job there and deletes the box when the
job is done. No job ever sees another job's files, processes or Docker images, and nothing has to be cleaned
up between jobs.

It is off by default. You turn it on with the `ci.github` section of the daemon config. The host needs no
inbound connection: it can poll GitHub for queued jobs, or receive webhooks if you can expose a port.

## How it works

1. **A job is queued** on GitHub with `runs-on: [self-hosted, davit]`. The host learns about it from a
   `workflow_job` webhook or from polling.
2. **The host checks the job.** Its labels must ask for this host. Its repository must be on the allowlist,
   must not be public (unless allowed) and the run must not come from a fork (unless allowed). A refused job is
   recorded with the reason, and gets no box.
3. **It waits for room.** At most `maxConcurrentJobs` runner boxes live at once; later jobs wait in a FIFO
   queue. Then the box waits in the host's own capacity queue for memory and disk, like any other box.
4. **The box starts.** The host forks a box from the runner image: a restore from the image's golden memory
   snapshot when one exists (`docs/research/SPIKE.md`), else a cold boot that captures one for the next job.
5. **The runner starts.** The host asks GitHub for a just-in-time runner configuration and starts
   `/opt/actions-runner/run.sh` in the box as the unprivileged user `runner`. The configuration goes to the
   runner in its environment only; the host never stores it.
6. **GitHub gives the runner a job** and the runner runs it. Its output is kept in the box's process history.
7. **The box is deleted** when the runner exits (a just-in-time runner exits after one job), when the job
   timeout passes (default 6 hours), or a short while after GitHub reports the job completed if the runner
   has not exited by then. The runner's registration goes too.

GitHub gives a queued job to *any* idle runner whose labels match, not necessarily to the one registered for
it. So a runner may run a different job of the same repository than the one it was made for. The host
follows this: when GitHub says which runner took which job, a runner given a job the policy refuses (a fork's,
say) has its box deleted at once. When a runner gets another job than its own, the idle runner made for
the job it took serves its own instead, if it can (same repository and shape); otherwise a new runner is
started for it. A runner whose job was cancelled before it got one is removed along with its box.

## Create the GitHub App

The host acts as a GitHub App that you create and install. Nothing else needs a token.

1. Open **Settings → Developer settings → GitHub Apps → New GitHub App** (for an organization: the
   organization's settings → Developer settings).
2. Name it, and set any homepage URL.
3. **Webhook:** to receive webhooks, tick *Active*, set the URL GitHub will post to (see
   [Webhook or polling](#webhook-or-polling)) and a random secret (`openssl rand -hex 32`). For polling only,
   untick *Active*.
4. **Permissions**, for runners registered in each repository (the default):
   - Repository → **Actions: Read** (queued jobs, runs, whether a run comes from a fork)
   - Repository → **Administration: Read and write** (register and remove runners)
   - Repository → **Metadata: Read** (selected automatically)

   For runners registered in an organization's runner group (`"runnerScope": "org"`), add Organization →
   **Self-hosted runners: Read and write**; Actions and Metadata are still needed.
5. **Subscribe to events:** tick **Workflow job** (only with an active webhook).
6. Create the App. Note its **App ID**. Under **Private keys**, generate a key; GitHub downloads a `.pem`
   file. Put it on the host and make it owner-only:

   ```sh
   install -m 600 ~/Downloads/your-app.*.private-key.pem ~/.config/davit/github-app.pem
   ```

7. **Install** the App (*Install App*) on your account or organization, for the repositories that should run
   jobs here. The installation ID is the number at the end of the installation's settings URL
   (`https://github.com/settings/installations/12345678`, or `/organizations/ORG/settings/installations/…`).

## Webhook or polling

Both find the same jobs; you can use either or both.

- **Polling** (the default, every 30 s): the host lists each allowed repository's queued and in-progress
  runs and their jobs. It needs no inbound connection. Each poll costs one or two API requests per
  repository plus one per active run, out of the installation's 5,000 requests per hour. A job starts up
  to `pollInterval` after it was queued.
- **Webhooks:** GitHub posts each `workflow_job` event to the host, so a job starts within a second or two
  of being queued. The receiver (`webhook.listen`) only accepts deliveries signed with the secret
  (`X-Hub-Signature-256`). GitHub must be able to reach it: run it on `127.0.0.1` behind a tunnel or reverse
  proxy that has a public HTTPS URL (Cloudflare Tunnel, Tailscale Funnel, a proxy on a public host), and give
  that URL to the App. Keep polling on as a fallback: GitHub does not redeliver a webhook that failed.

## Configuration

`ci.github` in the daemon config (`davit doctor` checks the key, the webhook secret and the image):

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Turns the runners on |
| `appId`, `privateKeyFile` | — | The App's ID and its `.pem` key (absolute path, mode 0600) |
| `installations` | — | `[{"id": INSTALLATION_ID, "repos": [{"name": "owner/repo"}]}]`. Only these repositories run jobs here |
| `repos[].allowPublic` | `false` | Let a public repository run jobs ([Security](#security)) |
| `repos[].allowForks` | `false` | Run jobs of pull requests from forks ([Security](#security)) |
| `webhook.listen`, `webhook.secretFile` | — | Webhook receiver address and its secret file (mode 0600); empty: no receiver |
| `pollInterval` | `30s` | How often to poll; at least `5s`; negative: never (webhooks only) |
| `label` | `davit` | The `runs-on` label that marks a job for this host |
| `shapes`, `defaultShape` | built-in `davit-default` | Box sizes selectable with one more label ([Shapes](#shapes)) |
| `image` | `ubuntu-24.04-ci` | The image runner boxes start from |
| `maxConcurrentJobs` | `4` | Runner boxes alive at once; the rest queue |
| `jobTimeout` | `6h` | How long a runner may run before its box is deleted (`1m`..`120h`) |
| `runnerScope`, `runnerGroupId` | `repo` | `org` registers runners in an organization's runner group (default group 1) |
| `runnerNamePrefix` | `davit-` + host name | Start of every runner name. Two hosts must not share it |
| `apiUrl` | `https://api.github.com` | GitHub Enterprise Server: `https://HOST/api/v3` |
| `runnerCommand`, `runnerUser` | `/opt/actions-runner/run.sh`, `runner` | For a custom runner image with another layout |

### A full host config

A 16-core, 128 GB host that runs only CI ([sizing](#sizing-a-16-core-128-gb-host)), with webhooks and
polling:

```json
{
  "dataDir": "/home/ci/.local/share/davit",
  "socket": "/run/user/1000/davit/davit.sock",
  "runtime": "firecracker",
  "limits": {
    "cpus": 32,
    "memoryBytes": 120259084288,
    "memoryOvercommit": 1,
    "minFreeDiskPercent": 10
  },
  "ci": {
    "github": {
      "enabled": true,
      "appId": 123456,
      "privateKeyFile": "/home/ci/.config/davit/github-app.pem",
      "installations": [
        {
          "id": 12345678,
          "repos": [
            {"name": "acme/api"},
            {"name": "acme/web"},
            {"name": "acme/sdk", "allowPublic": true}
          ]
        }
      ],
      "webhook": {
        "listen": "127.0.0.1:8475",
        "secretFile": "/home/ci/.config/davit/github-webhook-secret"
      },
      "pollInterval": "60s",
      "shapes": [
        {"label": "davit-4c-8g", "cpus": 4, "memoryMiB": 8192, "diskGiB": 64},
        {"label": "davit-8c-16g", "cpus": 8, "memoryMiB": 16384, "diskGiB": 96},
        {"label": "davit-16c-32g", "cpus": 16, "memoryMiB": 32768, "diskGiB": 128}
      ],
      "defaultShape": "davit-4c-8g",
      "maxConcurrentJobs": 8,
      "jobTimeout": "3h"
    }
  }
}
```

### Polling only

The smallest working config: one private repository, no inbound connection.

```json
{
  "dataDir": "/home/ci/.local/share/davit",
  "socket": "/run/user/1000/davit/davit.sock",
  "runtime": "firecracker",
  "ci": {
    "github": {
      "enabled": true,
      "appId": 123456,
      "privateKeyFile": "/home/ci/.config/davit/github-app.pem",
      "installations": [{"id": 12345678, "repos": [{"name": "acme/api"}]}]
    }
  }
}
```

Restart the daemon after changing `ci.github`.

## Use it in a workflow

Change `runs-on`:

```yaml
jobs:
  test:
    runs-on: [self-hosted, davit]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: stable}
      - run: go test ./...
```

A job is this host's when its labels include `self-hosted` and the host label (`davit`), and every other
label is one a davit runner has: `linux`, the host's architecture (`x64` or `arm64`), or one shape label.
Labels match case-insensitively, as on GitHub. A job with any other label (`gpu`, `macos`, `ubuntu-latest`)
is left for other runners, and the host does not record it.

A matrix runs one box per job, in parallel up to `maxConcurrentJobs`.

## Shapes

Without a shape label a job gets the default shape: the built-in `davit-default` (4 vCPUs, 8 GiB memory,
64 GiB disk), or the one `defaultShape` names. To pick another, add its label:

```yaml
    runs-on: [self-hosted, davit, davit-8c-16g]
```

A job may name at most one shape, and only a configured one; otherwise it is not this host's. The disk size
is the box's virtual disk: it is sparse, so a job uses only the space it writes, but the host's disk
admission reserves part of it (`limits.diskHeadroomPercent`) while the box lives.

## Security

A CI job runs whatever code the workflow checks out, with that repository's secrets. The defaults keep that
to code from repositories you control:

- **Allowlist.** Only repositories listed under `installations` run jobs. A job from any other repository
  the App is installed on is refused.
- **Public repositories are refused** unless `allowPublic` is set for that repository. On a public
  repository anyone can open a pull request, and workflow changes in it can reach the runner.
- **Forks are refused.** A run whose code comes from another repository (a `pull_request` or
  `pull_request_target` from a fork) is refused unless `allowForks` is set for the repository. The host also
  checks the job GitHub actually gives each runner: a runner given a refused job has its box deleted before
  the job can do much. That check needs the `in_progress` event, so it is up to `pollInterval` late without
  webhooks. For defense in depth, keep GitHub's own setting for fork pull request workflows strict
  (repository or organization **Settings → Actions → General**).
- **Organization runner groups.** With `"runnerScope": "org"`, GitHub may give a runner a job from any
  repository the runner group allows. Limit the group to the allowed repositories and leave *Allow public
  repositories* off; the host still refuses and deletes the box of any job from a repository not on its list.
- **One job, one box.** Each job gets a new box from the image and the box is deleted afterwards, also when
  the job fails, times out or the daemon restarts. Nothing a job leaves behind reaches the next one.
- **The runner is unprivileged** in the box (user `runner`). Like GitHub's hosted runners it has
  passwordless `sudo` and is in the `docker` group, so a job is root inside its own box; the box is the
  boundary.
- **Network.** Runner boxes get the host's default egress policy: by default `filtered`, so the internet
  is reachable but the LAN, the host, cloud metadata addresses and the tailnet are not
  ([network.md](network.md)). They get no credentials from the host's credential proxy.
- **Secrets on the host.** The App key and the webhook secret must be owner-only files. A runner's
  just-in-time configuration is passed to the runner in its environment and never written to the host's
  state, events or logs; the runner removes it from the environment its job sees. The registration is
  single-use and GitHub removes it after the job.

## The runner image

`ubuntu-24.04-ci` is the `ubuntu-24.04-dev` image plus the GitHub Actions runner, Docker and the usual build
tools. [images.md](images.md#ubuntu-2404-ci-github-actions-runners) lists what it has and what it lacks
compared with GitHub's `ubuntu-latest`. To build it on a VM host from source and install it beside the
default image ([vm-host.md](vm-host.md#installing-davit-on-the-host-from-source)):

```sh
make host-install HOST=user@host HOST_INSTALL_FLAGS='--image ubuntu-24.04-dev --image ubuntu-24.04-ci'
```

GitHub stops sending jobs to runners that are too far behind the current runner release. The runner
version is pinned in `images/inputs.lock`; bump it and rebuild the image when GitHub releases a new one.

## Caching

Every job starts from the same clean image, so nothing is cached between jobs on the host:

- `actions/cache` still stores and restores through GitHub's cache service, over the internet, the same as
  on GitHub-hosted runners. It counts against the repository's cache quota.
- `actions/setup-node`, `setup-go` and `setup-python` download their toolchain into the runner's tool cache
  (`/opt/hostedtoolcache`) in every job. The image already has Go, Node.js and Python on the `PATH`; a job
  that can use them can skip the setup step.
- Docker starts with an empty image store in each job. Use registry caching (`--cache-from`,
  `cache-to: type=registry`) or GitHub's cache backend for BuildKit (`type=gha`).
- To keep dependencies warm, bake them into a custom image derived from `ubuntu-24.04-ci` and point
  `ci.github.image` at it.

## Watching it

```sh
davit ci status            # queued, running and recent jobs: repo, job, box, duration, result
davit ci status --json     # the same as JSON (getCIStatus)
davit events --types ci.job.
davit history VM PROCESS   # a runner's output (VM and PROCESS from `ci status --json`)
davit doctor               # ci_github_app, ci_github_webhook, ci_image
```

Events: `ci.job.created` (a job was queued here), `ci.job.state` (queued → starting → running → stopping →
done) and `ci.job.refused` (`error` is the code; `davit ci status` shows the reason); see
[events.md](events.md). A done job's `result` is `succeeded` (the runner exited 0; `conclusion` is the job's
own result), `failed`, `cancelled`, `timedOut` or `refused`.

Restarting the daemon does not lose jobs: each job's progress is journaled, and a restarted daemon resumes
it. On start, and every 10 minutes, the host also deletes runner boxes that no job accounts for and removes
idle runner registrations with its `runnerNamePrefix` that no job will use.

## Sizing a 16-core, 128 GB host

These are planning numbers, not measurements. A Ryzen 9 9950X has 16 cores and 32 threads.

- **Memory.** Leave about 16 GiB for the host and the page cache, so `limits.memoryBytes` ≈ 112 GiB. Builds
  use the memory they are given, so set `limits.memoryOvercommit` to `1` on a CI host (the default 1.5
  counts on boxes sharing their golden's pages, which compiles and test runs do not).
- **CPU.** `limits.cpus` caps the sum of live boxes' vCPUs. Compiles keep their vCPUs busy; 32 (one per
  hardware thread) is a reasonable start. More oversubscribes the cores.
- **Concurrency.** With the default 4 vCPU / 8 GiB shape, 8 jobs use 32 vCPUs and 64 GiB; the memory left
  lets a few larger shapes run beside them. `maxConcurrentJobs: 8` fits that. Jobs beyond it, or beyond the
  host limits, queue instead of failing.
- **Disk.** Each box's disk is a reflink clone of the image, so a box costs only what its job writes. Plan
  for the largest job outputs times the concurrency, plus the images, goldens (one per image and shape) and
  the free-space floor: an NVMe drive of 1 TB or more, formatted XFS with reflink or btrfs
  ([vm-host.md](vm-host.md)).

Compared with GitHub-hosted runners, a box from a golden starts in seconds, but every job downloads what
`actions/cache` and the setup actions fetch, over the host's internet connection. Size the uplink for that.

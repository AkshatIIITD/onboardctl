# onboardctl

One command to onboard a new project onto the platform. It is idempotent, so you can safely re-run it.

```bash
onboardctl -name new-project
```

## What it creates

For `-name new-project` (group root defaults to `/harbor/project`):

| # | System | What |
|---|--------|------|
| 1 | OpenShift | namespace/project `new-project` (via `oc`) |
| 2-5 | Keycloak | groups `/harbor/project/new-project/{admin,maintainer,developer,guest}` (missing parents are created) |
| 6 | Harbor | **private** project `new-project` |
| 7-10 | Harbor | those four groups added as members: admin→project admin, maintainer, developer, guest |
| 11 | Harbor | robot account `pull-push` (repository pull+push, never expires) → login `robot$new-project+pull-push` |
| 12 | OpenShift | image pull secret `harbor-pull` built from the robot credential |
| 13 | OpenShift | `harbor-pull` linked to the `default` service account for pull |
| 14 | OpenShift | service account `ci-cd`, RoleBinding to the `edit` ClusterRole, and a long-lived token secret `ci-cd-token` |

It does **not** create ResourceQuotas or LimitRanges, and it does not add users. Add users to the Keycloak groups by hand; their Harbor access follows at next login.

### The one-time robot secret

Harbor shows a robot's secret **once**. onboardctl prints it at the end of the run (even if a later step failed), or saves it with `-secrets-file creds.txt` (new file, mode 0600, never overwritten). Put it into your CI/CD variables together with the username. Progress lines never contain it.

If the robot already exists but the `harbor-pull` secret is missing (for example a run failed before step 12 and the secret was lost), the run stops with instructions: delete the robot in Harbor (project > Robot Accounts) and re-run.

### The ci-cd token

Not printed. Read it when you need it:

```bash
oc get secret ci-cd-token -n new-project -o jsonpath='{.data.token}' | base64 -d
```

For the pipeline you also need the API server URL (`oc whoami --show-server`). The `edit` role can read every secret in the namespace; a narrower Role (get/patch on deployments) is safer if the pipeline only runs `oc set image`.

## Prerequisites

1. **OpenShift:** `oc login` already done. Default mode `project` uses `oc new-project`, which works with the `self-provisioner` role. Use `-openshift-mode namespace` only if you may create Namespace objects directly.
2. **Harbor:** auth mode must be **OIDC** (Keycloak), with the *Group Claim Name* set (e.g. `groups`). The credentials need permission to create projects and manage their members.
3. **Keycloak:** a confidential client (the tool fetches and refreshes short-lived tokens itself from the client ID and secret; do not paste a token) with *Service accounts enabled* whose service account has the realm-management role `manage-users` in the target realm. The Keycloak client needs a *Group Membership* mapper with **Full group path = ON**, added to the Harbor client, so tokens carry `/harbor/project/new-project/admin` exactly as Harbor stores it.
4. `-keycloak-realm` is the realm **name** (as in the URL), not its display name. Older Keycloak needs `/auth` on the URL.

## Settings

Secrets are read from the environment only, so they stay out of shell history and `ps`.

| Env var | Flag | Notes |
|---|---|---|
| `HARBOR_URL` | `-harbor-url` | |
| `HARBOR_USERNAME` | `-harbor-user` | |
| `HARBOR_PASSWORD` | (none) | secret |
| `KEYCLOAK_URL` | `-keycloak-url` | |
| `KEYCLOAK_REALM` | `-keycloak-realm` | realm name |
| `KEYCLOAK_TOKEN_REALM` | `-keycloak-token-realm` | only if the client lives in another realm (e.g. `master`) |
| `KEYCLOAK_CLIENT_ID` | `-keycloak-client-id` | |
| `KEYCLOAK_CLIENT_SECRET` | (none) | secret |
| `ONBOARDCTL_CA_FILE` | `-ca-file` | internal CA PEM for Harbor and Keycloak |
| `HARBOR_REGISTRY` | `-harbor-registry` | host[:port] used in image names; default is the host of `-harbor-url` |

Other flags: `-name` (required), `-group-root`, `-harbor-storage-gib`, `-robot-days` (0 = never expires), `-secrets-file`, `-openshift-mode`, `-oc-binary`, `-oc-context`, `-insecure`, `-dry-run`.

## Safe first run

```bash
go run ./cmd/onboardctl -name test-onboard -dry-run   # no credentials needed
go run ./cmd/onboardctl -name test-onboard            # throwaway name first
```

## Behaviour worth knowing

- Existing things are left unchanged, including a Harbor member's role.
- A failure stops the run; fix it and re-run the same command.
- If `oc` says a project does not exist but then says it already exists, someone else owns that name and you cannot see it. Pick another name.
- Names must be lowercase DNS labels (`a-z`, `0-9`, `-`, max 63) because they are shared by all three systems.

## Develop

```bash
gofmt -l . && go vet ./... && go test -race -cover ./...
```

Layout: `cmd/onboardctl` (flags), `internal/onboard` (plan and execute), `internal/{harbor,keycloak,openshift}` (clients; `robot.go` and `resources.go` hold the robot and pull-secret/ci-cd code), `internal/httpx` (TLS client), `internal/validate`.

## Exercises

1. Add a `-delete` mode that removes a project (think about what must be empty first).
2. Add a `-members-file` flag that reads usernames and adds them to Keycloak groups.
3. Print a JSON summary with `-o json` for use in CI.

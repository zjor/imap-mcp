# Kubernetes deployment and CD

Date: 2026-11-17. Status: approved.

## Intent

Run imap-mcp on the operator's Kubernetes cluster and deploy it from GitHub Actions. The repo is public, so it must not contain the domain, the IMAP host or any token. The setup follows the operator's other projects (`accounting`, `qr-share`): a Helm chart under `deploy/`, a namespace-scoped deployer ServiceAccount, and a deploy job in GitHub Actions.

## Goals

- `helm upgrade --install` of imap-mcp behind ingress-nginx with a cert-manager certificate.
- Automatic deploy of every release: a pushed tag `v*` builds the release, then deploys it.
- Manual deploy of any released version, for rollback.
- All site-specific values come from GitHub secrets at deploy time.

## Non-goals

- A second image pipeline. The image is the one goreleaser already pushes to `ghcr.io/zjor/imap-mcp`.
- Deploy on every push to `master`.
- Multiple environments (staging and production).
- A Helm repository or a published chart for other users.

## Anti-goals

- The domain, IMAP host or tokens appear in git, in workflow logs or in the Helm release values that the chart prints.
- The ingress logs request headers. They carry `X-IMAP-Password`.
- The deployer identity can touch anything outside its namespace.

## Approaches

- **A. Deploy every push to `master`.** CI builds an image tagged with the short SHA and deploys it, like `accounting`. Needs a second build next to goreleaser.
- **B. Deploy each release tag (chosen).** The deploy job runs after the existing `release` job and uses `ghcr.io/zjor/imap-mcp:<version>`. Production runs the same image as every other user. No new build and no Docker Hub.

The operator chose B.

## Design

### Files

```
deploy/
  chart/
    Chart.yaml
    values.yaml            # placeholders only: mail-mcp.example.com
    templates/
      deployment.yaml
      service.yaml
      ingress.yaml
  cd/rbac.yaml             # ServiceAccount github-deployer + Role + RoleBinding + token Secret
  scripts/
    create-secrets.sh      # manual fallback: k8s Secret from deploy/.env
    deploy-with-helm.sh    # manual fallback: helm upgrade with a given version
  .env.example
  README.md                # runbook
```

`deploy/.env` is git-ignored.

### Chart

- Names: release `imap-mcp`, namespace `imap-mcp`, kube context `midgard` (scripts only; CI uses the kubeconfig secret).
- Image `ghcr.io/zjor/imap-mcp`, tag set at deploy time. The tag is the version without the `v` prefix, as goreleaser publishes it.
- One replica. The server is stateless, so the replica count can grow later without changes.
- Pod security as in `accounting`: non-root uid 65532, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp.
- Probes: `GET /healthz` for liveness and readiness.
- Plain env from values: `IMAP_MCP_INSECURE_HTTP=true`, `IMAP_MCP_PUBLIC_URL=https://<ingress.host>`, `IMAP_MCP_MAX_UPLOAD`, `IMAP_MCP_LOG_LEVEL`.
- Secret env through `envFrom` on Secret `imap-mcp-secrets`: `IMAP_MCP_IMAP_HOSTS`, `IMAP_MCP_ACCESS_TOKENS`. The IMAP host is in the Secret because it names the mail provider.
- Ingress, class `nginx`, cluster issuer `letsencrypt-prod`, with these annotations:
  - `proxy-body-size: 30m` (default upload limit 25 MB plus multipart overhead);
  - `proxy-buffering: "off"` and `proxy-request-buffering: "off"` (downloads and uploads stream);
  - `proxy-read-timeout` and `proxy-send-timeout: "600"` (long transfers; one IMAP session lives at most 10 minutes).

### CI/CD

A new `deploy` job in `.github/workflows/ci.yml`:

- Runs after `release` on a tag `v*`. Also runs on `workflow_dispatch` with a `version` input, for rollback or a redeploy.
- Uses GitHub environment `production`, so fork workflows never see the secrets.
- Steps:
  1. write the kubeconfig from `KUBECONFIG`;
  2. apply Secret `imap-mcp-secrets` from GitHub secrets (`kubectl create secret --dry-run=client -o yaml | kubectl apply -f -`);
  3. `helm upgrade --install --atomic --timeout 5m` with `image.tag` and `ingress.host`;
  4. check that `https://<host>/healthz` returns `ok` and the Deployment runs the expected image.
- One deploy at a time (`concurrency`, no cancel).
- Secret values never go on a command line that the workflow echoes. The host is masked with `::add-mask::`, because it is passed with `--set`.

GitHub secrets (environment `production`):

| Secret | Value |
|---|---|
| `KUBECONFIG` | base64 kubeconfig for ServiceAccount `github-deployer` |
| `INGRESS_HOST` | public host name |
| `IMAP_MCP_IMAP_HOSTS` | allowed IMAP `host:port` list |
| `IMAP_MCP_ACCESS_TOKENS` | comma-separated access tokens |

### RBAC

`deploy/cd/rbac.yaml` copies the `accounting` one with namespace `imap-mcp`, minus `batch/jobs` (no migrate hook). The operator applies it once with an admin kubeconfig. The runbook has the kubeconfig recipe from `accounting` (direct API endpoint, not the Rancher proxy).

## Risks and checks

- **GHCR visibility.** No release exists yet. A new GHCR package can be private even for a public repo. The runbook says to check an anonymous pull after the first release and to make the package public if needed. Otherwise the cluster needs an image pull secret.
- **Header logging.** The default ingress-nginx log format has no custom request headers (inferred, not checked on the cluster). The runbook asks to check the controller's `log-format-upstream` once.
- **Local cluster access.** The local `midgard` context now fails with `system:unauthenticated` (Rancher token expired), so cluster facts (ingress class, issuer name) are taken from `accounting`, not checked.

## Left to the builder

- Exact probe timings and resource requests.
- The wording of the runbook.

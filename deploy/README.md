# Deploy to Kubernetes

This folder deploys imap-mcp to a Kubernetes cluster with Helm. GitHub Actions deploys every release. The repo holds no site-specific values: the host name, the IMAP host and the tokens live only in GitHub secrets (or in a local, git-ignored `deploy/.env`).

The cluster needs ingress-nginx and cert-manager with a `ClusterIssuer` named `letsencrypt-prod`.

## What is here

| Path | Purpose |
|---|---|
| `chart/` | Helm chart: Deployment, Service, Ingress. `ingress.host` and `image.tag` are required at deploy time. |
| `cd/rbac.yaml` | ServiceAccount `github-deployer`, limited to namespace `imap-mcp` |
| `scripts/create-secrets.sh` | manual fallback: writes Secret `imap-mcp-secrets` from `deploy/.env` |
| `scripts/deploy-with-helm.sh` | manual fallback: `helm upgrade --install` of a given version |
| `scripts/ci-validate.sh` | input checks for the CI deploy job |
| `scripts/check-chart.sh`, `scripts/test-ci-validate.sh` | local checks, run by `make chart-check` |
| `.env.example` | template for `deploy/.env` |

Names: release `imap-mcp`, namespace `imap-mcp`, Secret `imap-mcp-secrets`. The scripts use kube context `midgard`; set `KUBE_CONTEXT` to use another one.

## One-time setup

1. Create the namespace and the deployer identity with an admin kubeconfig:

   ```sh
   kubectl --context midgard create namespace imap-mcp
   kubectl --context midgard apply -f deploy/cd/rbac.yaml
   ```

2. Build a kubeconfig for `github-deployer` and store it as a GitHub secret. Use the cluster's direct API endpoint. A Rancher proxy URL (`.../k8s/clusters/...`) accepts only Rancher tokens and rejects ServiceAccount tokens as `system:unauthenticated`.

   ```sh
   CTX=midgard; NS=imap-mcp
   SERVER="https://$(kubectl --context $CTX -n default get endpoints kubernetes -o jsonpath='{.subsets[0].addresses[0].ip}'):6443"
   CA="$(kubectl --context $CTX -n $NS get cm kube-root-ca.crt -o jsonpath='{.data.ca\.crt}' | base64 | tr -d '\n')"
   TOKEN="$(kubectl --context $CTX -n $NS get secret github-deployer-token -o jsonpath='{.data.token}' | base64 -d)"
   umask 077
   cat > /tmp/cd-kubeconfig.yaml <<EOF
   apiVersion: v1
   kind: Config
   clusters:
   - name: $CTX
     cluster: {server: $SERVER, certificate-authority-data: $CA}
   users:
   - name: github-deployer
     user: {token: $TOKEN}
   contexts:
   - name: $CTX
     context: {cluster: $CTX, user: github-deployer, namespace: $NS}
   current-context: $CTX
   EOF
   KUBECONFIG=/tmp/cd-kubeconfig.yaml kubectl -n $NS get secrets   # must work
   base64 < /tmp/cd-kubeconfig.yaml | tr -d '\n' | gh secret set KUBECONFIG --env production
   rm /tmp/cd-kubeconfig.yaml
   ```

   Rebuild it when the token is rotated or the API endpoint changes.

   **If this kubeconfig leaks**, recreating only `github-deployer-token` is not enough. The deployer may create Secrets, so a token holder can mint another token for the ServiceAccount. Delete and recreate the ServiceAccount, which revokes all its tokens, then repeat this step:

   ```sh
   kubectl --context midgard -n imap-mcp delete -f deploy/cd/rbac.yaml
   kubectl --context midgard apply -f deploy/cd/rbac.yaml
   ```

3. Create the GitHub environment `production` (repo Settings → Environments). Its deployment branches and tags rule must allow the tag pattern `v*`. A tag push runs the deploy job on a tag ref, not on a branch. So a rule like "Protected branches only" blocks every release deploy. Use "No restriction", or "Selected branches and tags" with a tag rule `v*`. Rollbacks also run on a tag ref (`--ref v0.1.0`), so the same rule covers them.

   Then set the other secrets in this environment:

   | Secret | Value |
   |---|---|
   | `KUBECONFIG` | base64 kubeconfig for `github-deployer` (step 2) |
   | `INGRESS_HOST` | public host name, no scheme, for example `mail-mcp.example.com` |
   | `IMAP_MCP_IMAP_HOSTS` | allowed IMAP servers, for example `imap.example.com:993` |
   | `IMAP_MCP_ACCESS_TOKENS` | comma-separated tokens, one per person (`openssl rand -hex 32`) |

   ```sh
   gh secret set INGRESS_HOST --env production
   gh secret set IMAP_MCP_IMAP_HOSTS --env production
   gh secret set IMAP_MCP_ACCESS_TOKENS --env production
   ```

   `gh secret set` asks for the value, so it does not go into your shell history.

4. Point DNS for the host at the ingress controller.

## Deploy

Push a release tag. CI runs the tests, the `release` job publishes `ghcr.io/zjor/imap-mcp:X.Y.Z`, and the `deploy` job runs:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The `deploy` job:

1. checks the inputs (`scripts/ci-validate.sh`); an empty secret or a bad host fails here, before the cluster is touched;
2. writes Secret `imap-mcp-secrets` from the GitHub secrets;
3. runs `helm upgrade --install --atomic`; a failed rollout rolls back;
4. restarts the pods, so a changed token list takes effect;
5. checks the running image and `https://<host>/healthz`.

**Rollback or redeploy** of a released version. Run the workflow from the tag, so the chart matches the image:

```sh
gh workflow run ci.yml --ref v0.1.0 -f version=0.1.0
```

**Change tokens:** update `IMAP_MCP_ACCESS_TOKENS` in GitHub, then redeploy the current version with the command above.

**First release:** the `deploy` job checks that the image can be pulled without login. A new GHCR package can be private even for a public repo, so the first deploy may fail at "Check image is pullable". Make the package public (see below), then redeploy with the command above.

## Manual fallback

When GitHub Actions is down:

```sh
cp deploy/.env.example deploy/.env      # fill it in; it is git-ignored
deploy/scripts/create-secrets.sh
deploy/scripts/deploy-with-helm.sh 0.1.0
```

## Checks after the first deploy

1. **The image is public.** Log out and pull:

   ```sh
   docker logout ghcr.io && docker pull ghcr.io/zjor/imap-mcp:0.1.0
   ```

   If this fails, open the package settings on GitHub and set the visibility to public. Otherwise the cluster cannot pull the image.

2. **The ingress does not log request headers.** They carry `X-IMAP-Password`. The default ingress-nginx log format has no custom request headers. Check that nobody changed it:

   ```sh
   kubectl -n ingress-nginx get cm -o yaml | grep -E 'log-format|http-snippet'
   ```

   No output means the defaults. A custom format must not contain `$http_x_imap_password` or `$http_authorization`.

3. **The server answers:**

   ```sh
   curl https://<host>/healthz   # prints ok
   ```

Then connect an agent with [docs/agent-setup.md](../docs/agent-setup.md), using `https://<host>/mcp`.

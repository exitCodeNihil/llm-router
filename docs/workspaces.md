# Workspaces

A workspace is a container you own with VS Code, a terminal and the [pi](https://pi.dev) coding
agent preconfigured (type `pi`). It is optional and off by default.

## Set up

Pick how you run llm-router.

### Docker Compose, from a clone

```bash
./deploy/quickstart.sh --workspaces
```

On an existing install, run it again: it adds workspaces and keeps your data. Then open the console,
create your admin account on the setup screen (the admin token has no user to own a workspace),
and go to **Workspaces**.

### Docker Compose, published image

In the folder with your `docker-compose.yml` and `.env`:

```bash
curl -fsSLO https://raw.githubusercontent.com/exitcodenihil/llm-router/main/deploy/docker-compose.workspaces.yml
cat >> .env <<'EOF'
COMPOSE_FILE=docker-compose.yml:docker-compose.workspaces.yml
LLMR_CONTAINER_SOCKET=/var/run/docker.sock
EOF
docker compose up -d
curl -X PUT http://localhost:8080/api/settings/workspaces \
  -H "Authorization: Bearer $(sed -n 's/^LLMR_ADMIN_TOKEN=//p' .env)" -H 'Content-Type: application/json' \
  -d '{"enabled":true,"runtime":"docker","socket":"/var/run/docker.sock","gateway_url":"http://llmrouter:8080","memory_mb":2048,"cpus":2,"budget_usd":5,"max_per_user":5}'
```

Rootless podman: set `LLMR_CONTAINER_SOCKET` to `/run/user/<uid>/podman/podman.sock`. On macOS use
the path from `podman info --format '{{.Host.RemoteSocket.Path}}'`, without `unix://`.

### On the host

If the gateway runs as a binary on the host (`make run`), no overlay is needed. In **Workspace
settings** turn on **Enable workspaces**, pick **Docker / podman**, and set:

- **Container socket**: `/var/run/docker.sock`, or `/run/user/<uid>/podman/podman.sock` for rootless
  podman.
- **Gateway URL from inside a workspace**: `http://host.containers.internal:8080` on podman,
  `http://host.docker.internal:8080` on Docker Desktop.

Run the gateway on the host, not in a container, unless you use the Compose overlay above: the IDE is
published on the host's loopback, which a containerised gateway can't reach.

### Kubernetes

On top of the install in [Deploying](deploy.md#kubernetes):

```bash
helm upgrade --install llm-router ./deploy/helm/llm-router --set existingSecret=llm-router \
  --set workspaces.enabled=true,workspaces.ideOrigin=https://ide.example.com \
  --set ingress.enabled=true,ingress.host=router.example.com,workspaces.ideHost=ide.example.com
```

The chart adds a service account with a Role for pods, pods/exec, persistentvolumeclaims and
secrets in the release's namespace, so install into a namespace of its own. Then in **Workspace
settings** pick **Kubernetes**, set **Storage class** and **Disk (GB)**, and set the gateway URL to
`http://llm-router-llm-router:8080` (`<release>-llm-router`).

Kubernetes doesn't build images. Build the template Dockerfiles, push them where the cluster can
pull, and put those references in each template's **Image**. A local look without an ingress:
`kubectl port-forward svc/llm-router-llm-router 8080 8081` with `workspaces.ideOrigin=http://localhost:8081`.

Running the gateway outside the cluster instead? Set `LLMR_K8S_API`, `LLMR_K8S_TOKEN` (or
`LLMR_K8S_CLIENT_CERT` and `LLMR_K8S_CLIENT_KEY`), `LLMR_K8S_CA` and `LLMR_K8S_NAMESPACE`.

## Use

**Workspaces → New workspace** and pick a template: `base`, `python`, `node`, `go`, `java` or
`infra`. A template must be built first: admins open **Workspaces → Templates** and press **Build
image** (a few minutes the first time). They also edit the Dockerfiles there. Under **Your files**,
upload an ssh key, `.gitconfig` and similar once; every workspace of yours gets them in `$HOME`
(encrypted at rest, 1 MB each).

Admins can use workspaces right away. To let others, add users or teams under **Workspace
settings**; an empty list means admins only.

`/workspace` is a volume and `$HOME` lives inside it, so code, shell history, ssh keys, caches and
pi's sessions survive restarts. Anything outside `/workspace`, such as an `apt install`, is lost
when the container is recreated; put tools in the template instead.

## Security

- The gateway holds your container socket, which is root on the host. Enable workspaces only where
  you accept that.
- Each workspace's agent has its own gateway key with a daily budget (**Daily budget ($)**,
  default 5) and each user is capped (**Workspaces per user**, default 5). Disable the key to stop
  a runaway agent.
- Admins can list, stop and delete any workspace but can't read its files or use its terminal or agent.
- Containers drop all capabilities, run `no-new-privileges` with memory, CPU and pids caps, and never
  see the container socket.
- The network is not fenced. With the Compose overlay a workspace shares a private network with the
  gateway only, so it can't reach Postgres, but it can still reach the internet and whatever the
  host can. On a bare host or Kubernetes it can also reach your LAN, published ports and cloud
  metadata.
- Extensions come from Open VSX, not Microsoft's marketplace. A workspace image must ship
  `code-server`, `pi` and `tar`.

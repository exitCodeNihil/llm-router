# Workspaces

A workspace is a container you own with VS Code, a terminal and the [pi](https://pi.dev) coding
agent preconfigured (type `pi`). It is optional and off by default.

## Enable

In **Workspace settings** (admin), turn on **Enable workspaces** and pick a **Runtime**:

- **Docker / podman**: set **Container socket** (`/var/run/docker.sock`, or
  `/run/user/<uid>/podman/podman.sock` for rootless podman) and **Gateway URL from inside a
  workspace** (`http://host.containers.internal:8080`).
- **Kubernetes**: set **Storage class** and **Disk (GB)**, and use a Service address for the gateway
  URL. Give the gateway RBAC for pods, pods/exec, persistentvolumeclaims and secrets in its
  namespace (the Helm chart doesn't). Outside the cluster, set `LLMR_K8S_API`, `LLMR_K8S_TOKEN` (or
  `LLMR_K8S_CLIENT_CERT` and `LLMR_K8S_CLIENT_KEY`), `LLMR_K8S_CA` and `LLMR_K8S_NAMESPACE`.

Add the users and teams who may create workspaces; an empty list means admins only. Set
`LLMR_IDE_LISTEN` and `LLMR_IDE_ORIGIN` ([deploy](deploy.md#configuration)) so the IDE is served
from its own origin.

## Use

**Workspaces → New workspace** and pick a template: `base`, `python`, `node`, `go`, `java` or
`infra`. Admins edit templates and their Dockerfiles; on Docker the console builds the image, on
Kubernetes you build and push it. Under **Your files**, upload an ssh key, `.gitconfig` and similar
once; every workspace of yours gets them in `$HOME` (encrypted at rest, 1 MB each).

`/workspace` is a volume and `$HOME` lives inside it, so code, shell history, ssh keys, caches and
pi's sessions survive restarts. Anything outside `/workspace`, such as an `apt install`, is lost
when the container is recreated; put tools in the template instead.

## Security

- Each workspace's agent has its own gateway key with a daily budget (**Daily budget ($)**,
  default 5) and each user is capped (**Workspaces per user**, default 5). Disable the key to stop
  a runaway agent.
- Admins can list, stop and delete any workspace but can't read its files or use its terminal or agent.
- Containers drop all capabilities, run `no-new-privileges` with memory, CPU and pids caps, and never
  see the container socket.
- The network is not fenced. A workspace can reach whatever the host can: your LAN, published
  ports, cloud metadata. The gateway holding a container socket is host-level trust, so enable
  workspaces only where you accept that.
- Extensions come from Open VSX, not Microsoft's marketplace. A workspace image must ship
  `code-server`, `pi` and `tar`.

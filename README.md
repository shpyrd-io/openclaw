# OpenClaw on shpyrd

[OpenClaw](https://openclaw.ai), the open-source assistant that answers on
WhatsApp, Telegram, Discord and more, packaged as a project for
[shpyrd](https://github.com/shpyrd-io/shpyrd), installed from the
catalog's templates. The official image, plus one program in front of
it: a setup screen on first visit, then OpenClaw's own Control UI.

## What the person sees

1. The project is installed from the templates. Nothing is asked but a
   name; an OpenRouter key may be given, optionally.
2. The first visit shows **Set up OpenClaw**: a provider (OpenRouter,
   Anthropic, or any OpenAI-compatible server), the key, then the model,
   chosen from the list the provider answers with for that key.
3. From then on the address is OpenClaw's Control UI: chat, sessions,
   channels, config. `/setup` reopens the setup to change provider, key
   or model.
4. Channels come later. In the Control UI's channels settings, or from a
   shell on the project (`shpyrd shell <project>`) with `openclaw
   channels add --channel telegram --token <token>`; WhatsApp pairs with
   `openclaw channels login`.

The platform's sign-in is the only login. The project's access is
"authenticated", people with a role on the project; the gateway listens
on loopback inside the container in trusted-proxy mode, taking who the
person is from the edge's `X-Shpyrd-User` header, which this program
hands it as `X-Forwarded-User`. Browsers are approved as they come. Do
not set the project's access to public: the gateway would then take
anyone as "visitor".

## How it runs

`openclaw-setup` (this repository, Go, no dependencies) is the container's
process. OpenClaw's state is `/data/openclaw`, a directory the program
makes inside the project's disk. The setup page is `web/`, a Next
application on the shpyrd design system (`@shpyrd/ui`), compiled to
static files under `/setup` and embedded in the program; the program
answers its API under `/setup/api`. It listens on `$PORT` and:

- writes the gateway's standing configuration at every start (local, on
  loopback, trusted-proxy auth, browsers auto-approved) with `openclaw
  config set --batch-json`;
- until `/data/openclaw/.shpyrd/setup.json` exists, shows the setup and
  redirects everything else to it;
- writes the choice through OpenClaw's own commands: the key into the
  config's environment (`env.vars.<PROVIDER>_API_KEY`), a custom server as
  `models.providers.custom`, the model with `openclaw models set`;
- then runs `openclaw gateway` as a child, starting it again when it
  exits, and proxies to it on 127.0.0.1:18789. A setup run again
  restarts the gateway.

As PID 1 the program first runs itself again as a child and stays as its
init: it waits for the processes the gateway leaves behind, while the
program waits for its own children only.

## Updating OpenClaw

The `FROM ghcr.io/openclaw/openclaw:<tag>` line in the Dockerfile. Every
push to main is built and released on the projects that follow it.

## Developing

    (cd web && npm ci && npm run build)   # the page, embedded by the program
    go test ./...
    docker build -t openclaw-shpyrd .

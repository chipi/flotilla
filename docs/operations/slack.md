# Slack approvals

Plans that wait for a person are posted in one Slack channel by the host agent that has the connector
role; Approve and Reject answer them from Slack, within the limits in [security](../security.md) (T16).
flotillad holds no Slack credential: the agent does (ADR-0006, decided 4).

## What it does

- Every 15 seconds flotillad makes one ask, with a random single-use nonce, for each plan waiting
  for a person, and sends the ones not yet posted to the connector host over its connection.
- The connector posts each in the channel: the case's title, the action and target, the plan's
  hash and a link to the case. A plan Slack may approve has Approve and Reject buttons; one it may not
  (high risk, irreversible) says to answer in the UI or the CLI.
- A click reaches the connector over Socket Mode (an outbound websocket; no host opens a port). It
  acknowledges it at once and relays it to flotillad, which checks everything again and approves or
  rejects as the pinned person (`via=slack` in the log and on the case). The answer replaces the
  buttons on the message.
- Approvals, Autonomy's guardrails and the Decision box show Slack's state and the day's use of
  the cap.

## Set up

1. **The Slack app** (in the workspace, by its admin): <https://api.slack.com/apps> → Create New
   App → From a manifest, with:

   ```yaml
   display_information:
     name: flotilla
     description: Plans that wait for you, with Approve and Reject
   features:
     bot_user:
       display_name: flotilla
       always_online: false
   oauth_config:
     scopes:
       bot:
         - chat:write
   settings:
     interactivity:
       is_enabled: true
     socket_mode_enabled: true
     org_deploy_enabled: false
     token_rotation_enabled: false
   ```

   Then: Basic Information → App-Level Tokens → Generate, scope `connections:write` (the `xapp-…`
   token); Install App → Install to Workspace (the `xoxb-…` bot token); invite the app to the channel
   (`/invite @flotilla`).
2. **The ids:** the channel id (channel details, at the bottom: `C…`), the workspace id (`T…`, in the
   workspace's URL or `auth.test`), and each approver's member id (their profile → ⋯ → Copy member
   ID: `U…`).
3. **The tokens on the connector host,** never in git or a command line: each in its own file, owned
   by the agent's user, mode 600:

   ```bash
   sudo install -m 600 -o _flotilla /dev/null /etc/flotilla-host/slack-bot.token   # then paste into it
   sudo install -m 600 -o _flotilla /dev/null /etc/flotilla-host/slack-app.token
   ```

   The agent refuses a token file anyone else can read, or one that does not hold one token of its kind.
4. **The host's config** gains:

   ```json
   "slack": {"bot_token_file": "/etc/flotilla-host/slack-bot.token",
             "app_token_file": "/etc/flotilla-host/slack-app.token", "channel": "C0123456789"}
   ```

   and `deploy/host/install.sh upgrade` installs it.
5. **flotillad.yaml** gains (the Slack user ids mapped to flotilla users with the operator or admin
   role):

   ```yaml
   slack:
     connector_host: mini
     team_id: T0123456789
     ui_url: https://flotilla.<tailnet>.ts.net
     users: {U0123456789: marko}
     daily_cap: 10
   ```

   `-check` refuses a slack section without `hosts`, pins or `ui_url`, or with a user id that is
   not one.

## Verify

- flotillad logs `approvals in Slack` at start; the agent logs `slack socket connected`.
- Autonomy's guardrail reads "through mini's connector, 1 person pinned…".
- The next plan waiting for a person appears in the channel within 15 seconds; a click answers it and
  the message shows the answer.

## Not covered

- Only plans are posted: an escalated case without a plan is not.
- A message answered in the UI first keeps its buttons until someone clicks; the click then says
  "already answered".
- A click made while the connector is disconnected from flotillad waits in its queue (64) and is
  relayed when it reconnects; flotillad still checks it then.
- Rotating the tokens: write the new ones into the same files and restart the agent.

---
name: granular-github
description: Run authorized GitHub actions through the granular-github CLI — list/view/create/comment/edit/close issues and pull requests, clone/push repos — on behalf of the user. Every action is policy-checked by the granular authorization server and only runs if the user's subject holds a matching grant. Use this whenever the task involves GitHub and you should act through granular rather than calling GitHub or git directly, and to request access (a grant) when an action is denied.
---

# granular-github

`granular-github` runs GitHub operations through the **granular** authorization
server. Each operation is checked against policy and runs only if the user's
**subject** holds a matching, human-approved **grant**. You act under a per-box
subject token. Prefer this over calling `git`/GitHub directly for actions that
should be authorized on the user's behalf.

## Setup is already done in this box

- The resource server URL is in `~/.granular/github.yaml`.
- Your subject token is in `~/.granular/subject_token`.

So just run `granular-github <command>` — do **not** pass `--base-url` or
`--token`, and never put a token on the command line.

## 1. Discover what's available

```
granular-github catalog
```

Lists the resource types and their match fields, the actions and action groups,
every operation with its parameters (required ones starred), and the grant
templates. Add `--json` for machine-readable output. Consult this to get exact
operation names, parameter names, action names, and templates — don't guess.

## 2. Run an operation

Operations are sub-commands with typed flags. Representative examples (run
`granular-github catalog` for the full set and every parameter):

```
granular-github issue list   --repo owner/name [--state open|closed|all] [--limit 30]
granular-github issue view   --repo owner/name --number 12 [--comments]
granular-github issue create --repo owner/name --title "Title" [--body "..."] [--labels bug,p1] [--assignees alice]
granular-github issue comment --repo owner/name --number 12 --body "..."
granular-github issue edit   --repo owner/name --number 12 [--title ...] [--body ...] [--add-labels x] [--remove-labels y]
granular-github issue close  --repo owner/name --number 12 [--reason completed]
granular-github pull view    --repo owner/name --number 5 [--comments]
granular-github pull diff    --repo owner/name --number 5
granular-github pull create  --repo owner/name --title "..." --head feature --base main [--draft]
granular-github pull review  --repo owner/name --number 5 --event approve|request_changes|comment [--body ...]
granular-github pull merge   --repo owner/name --number 5 [--method merge|squash|rebase] [--sha <head-sha>]
granular-github clone        --repo owner/name
granular-github push         --repo owner/name
```

The result prints as JSON on success.

## 3. When an operation is denied

If a command prints **"Not authorized"**, the subject lacks the required grant.
Grants are approved by a human — you cannot self-authorize. Request the **least**
access that covers the task; mutating operations are content-scoped, so approving
one specific change does not authorize a different one. Use the action names,
resource type, and match fields from `granular-github catalog` (don't guess).

### Single grant — `request`

Builds, signs, and submits a grant request in one step and prints an approval URL:

```
granular-github request \
  --actions issue.create,issue.comment \
  --resource github.repo --match owner=OWNER,name=NAME \
  --approver USER_EMAIL
```

Or from a template (see `catalog` for names and bindings):

```
granular-github request --template <name> --bind owner=OWNER --bind name=NAME --approver USER_EMAIL
```

**Share the printed approval URL with the user**, wait until they confirm
approval, then re-run the original operation.

### Several grants in one approval — bundle with `granular`

When the task needs multiple grants (e.g. across repos or resource servers),
sign each to a file, then submit them as **one** proposal so the user approves
once:

```
granular-github sign --actions issue.create --resource github.repo --match owner=O,name=A --out /tmp/g1.json
granular-github sign --actions pull.merge   --resource github.repo --match owner=O,name=B --out /tmp/g2.json
granular --config ~/.granular/client.yaml propose /tmp/g1.json /tmp/g2.json --approver USER_EMAIL
```

`granular propose` (the cross-resource-server client) prints a single approval
URL covering all bundled requests. Pass `--config ~/.granular/client.yaml` (it is
injected into this box); share the URL, wait for approval, then retry.

Do not loop blindly — if an operation is still denied after approval, re-check
the action and scope against `granular-github catalog`.

## Rules

- Never pass `--token`/`--base-url` or place secrets on the command line; the box
  provides them.
- Don't fall back to raw `git`/GitHub for actions that should be authorized —
  request a grant instead.
- Scope grants tightly; ask for a new grant when the task needs different access.

# Demo audience upload files

Sample first-party CRM lists for the "bring your own customers" demo
(DEMO-VIDEO-RUNBOOK Scene 5 — the first-party data finale).

## diamond-intenders.csv

The Lumière Diamonds "diamond intenders" list — hashed user ids, one per line,
under a single `user_id` header (the format `POST /v1/api/audiences` expects).

- **Log in as:** `lumi-re-diamonds@adtech.local` / `admin` (Lumière Diamonds advertiser).
- **Upload via:** advertiser portal → **Audiences** → create a first-party audience
  → upload this CSV. (Programmatic equivalent: `POST /v1/api/audiences` as
  multipart `file=@diamond-intenders.csv` + `name`/`type=first_party`/`visibility`,
  or JSON `{name, type:"first_party", user_ids:[...]}`.)
- **The first row** (`bc8f3218…c9e9df`) is the **seeded demo persona's** hashed id —
  so after upload, targeting this segment resolves on a live auction when you browse
  a demo site as that persona (Trace Explorer shows `Audience resolved`). The other
  rows are `sha256(<sample>@example.com)` — they pad the member count realistically
  but won't resolve (no matching live user), exactly like a real CRM list where only
  some members are online.

Note: the baseline seed already plants a **"Diamond & Jewelry Intenders"**
first-party segment with the persona as a member (that's what `make demo-warm` /
`make demo-reset` verify). This file is for demonstrating the **upload** step live;
you don't need it just to show the audience resolving.

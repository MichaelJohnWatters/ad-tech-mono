```mermaid
graph LR
  linkStyle default fill:#ffffff

  subgraph diagram ["Container View: Ad Tech Platform"]
    style diagram fill:#ffffff,stroke:#ffffff

    1["<div style='font-weight: bold'>Advertiser</div><div style='font-size: 70%; margin-top: 0px'>[Person]</div><div style='font-size: 80%; margin-top:10px'>Runs campaigns, uploads<br />audiences, views reports.</div>"]
    style 1 fill:#08427b,stroke:#052e56,color:#ffffff
    2["<div style='font-weight: bold'>Publisher</div><div style='font-size: 70%; margin-top: 0px'>[Person]</div><div style='font-size: 80%; margin-top:10px'>Manages inventory/placements,<br />views yield + payouts.</div>"]
    style 2 fill:#08427b,stroke:#052e56,color:#ffffff
    3["<div style='font-weight: bold'>Staff / Ops</div><div style='font-size: 70%; margin-top: 0px'>[Person]</div><div style='font-size: 80%; margin-top:10px'>Platform ops, moderation,<br />incidents, impersonation.</div>"]
    style 3 fill:#08427b,stroke:#052e56,color:#ffffff
    4["<div style='font-weight: bold'>End user (browser / app)</div><div style='font-size: 70%; margin-top: 0px'>[Person]</div><div style='font-size: 80%; margin-top:10px'>Sees ads; fires<br />impression/click/view<br />beacons.</div>"]
    style 4 fill:#08427b,stroke:#052e56,color:#ffffff
    5["<div style='font-weight: bold'>Third-party / competitor DSPs</div><div style='font-size: 70%; margin-top: 0px'>[Software System]</div><div style='font-size: 80%; margin-top:10px'>External OpenRTB bidders the<br />exchange fans out to.</div>"]
    style 5 fill:#999999,stroke:#6b6b6b,color:#ffffff
    6["<div style='font-weight: bold'>Prebid Server</div><div style='font-size: 70%; margin-top: 0px'>[Software System]</div><div style='font-size: 80%; margin-top:10px'>Header-bidding demand the<br />publisher-adserver fans out<br />to.</div>"]
    style 6 fill:#999999,stroke:#6b6b6b,color:#ffffff
    7["<div style='font-weight: bold'>OIDC Identity Provider</div><div style='font-size: 70%; margin-top: 0px'>[Software System]</div><div style='font-size: 80%; margin-top:10px'>Per-account SSO (auth-code +<br />PKCE).</div>"]
    style 7 fill:#999999,stroke:#6b6b6b,color:#ffffff
    8["<div style='font-weight: bold'>Email (Mailpit / SES)</div><div style='font-size: 70%; margin-top: 0px'>[Software System]</div><div style='font-size: 80%; margin-top:10px'>Verification, reports,<br />notifications.</div>"]
    style 8 fill:#999999,stroke:#6b6b6b,color:#ffffff

    subgraph 9 ["Ad Tech Platform"]
      style 9 fill:#ffffff,stroke:#0b4884,color:#0b4884

      10["<div style='font-weight: bold'>Gateway</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Auth (JWT/SSO/API-key), RBAC,<br />HTMX portals, REST API, proxy<br />to internal gRPC.</div>"]
      style 10 fill:#0d9488,stroke:#0b7268,color:#ffffff
      100["<div style='font-weight: bold'>Identity Consumer</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Builds the identity graph<br />from observed IDs<br />(deterministic +<br />probabilistic).</div>"]
      style 100 fill:#c026d3,stroke:#9e20ae,color:#ffffff
      107["<div style='font-weight: bold'>Audience RT</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Real-time retargeting: enroll<br />on site-visit, suppress on<br />purchase.</div>"]
      style 107 fill:#a21caf,stroke:#851790,color:#ffffff
      115["<div style='font-weight: bold'>Report Runner</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Async report jobs (SKIP<br />LOCKED queue) → CSV/Parquet<br />in object storage; export<br />zips.</div>"]
      style 115 fill:#6366f1,stroke:#5255c7,color:#ffffff
      125["<div style='font-weight: bold'>Webhooks</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Delivers business events to<br />registered partner URLs.</div>"]
      style 125 fill:#b45309,stroke:#934407,color:#ffffff
      133["<div style='font-weight: bold'>Notifications</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>In-app portal notifications<br />(the bell).</div>"]
      style 133 fill:#d97706,stroke:#b36205,color:#ffffff
      137["<div style='font-weight: bold'>Batch Conductor</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go (CronJob)]</div><div style='font-size: 80%; margin-top:10px'>Hourly completion-ordered<br />data chain: rollups → export<br />→ profile-builder → privacy<br />purge.</div>"]
      style 137 fill:#155e75,stroke:#114d60,color:#ffffff
      145[("<div style='font-weight: bold'>PostgreSQL</div><div style='font-size: 70%; margin-top: 0px'>[Container: PostgreSQL]</div><div style='font-size: 80%; margin-top:10px'>Transactional store,<br />multi-tenant via RLS<br />(adtech_app NOBYPASSRLS).</div>")]
      style 145 fill:#438dd5,stroke:#2e6295,color:#ffffff
      146[("<div style='font-weight: bold'>Redis</div><div style='font-size: 70%; margin-top: 0px'>[Container: Redis]</div><div style='font-size: 80%; margin-top:10px'>L2: budget/freq-cap counters,<br />audience sets, sessions, rate<br />limits.</div>")]
      style 146 fill:#438dd5,stroke:#2e6295,color:#ffffff
      147[("<div style='font-weight: bold'>ClickHouse</div><div style='font-size: 70%; margin-top: 0px'>[Container: ClickHouse]</div><div style='font-size: 80%; margin-top:10px'>Analytics event store<br />(impressions, auctions,<br />auction_wins/losses/shades,<br />…).</div>")]
      style 147 fill:#438dd5,stroke:#2e6295,color:#ffffff
      148[("<div style='font-weight: bold'>NATS JetStream</div><div style='font-size: 70%; margin-top: 0px'>[Container: NATS]</div><div style='font-size: 80%; margin-top:10px'>Async event bus (JSON<br />payloads).</div>")]
      style 148 fill:#438dd5,stroke:#2e6295,color:#ffffff
      149[("<div style='font-weight: bold'>Object Storage</div><div style='font-size: 70%; margin-top: 0px'>[Container: S3]</div><div style='font-size: 80%; margin-top:10px'>Creatives + the Parquet/Delta<br />lake (Minio local / S3 prod).</div>")]
      style 149 fill:#438dd5,stroke:#2e6295,color:#ffffff
      150[("<div style='font-weight: bold'>TigerBeetle</div><div style='font-size: 70%; margin-top: 0px'>[Container: TigerBeetle]</div><div style='font-size: 80%; margin-top:10px'>Double-entry billing ledger<br />(spend reserve/settle).</div>")]
      style 150 fill:#438dd5,stroke:#2e6295,color:#ffffff
      18["<div style='font-weight: bold'>SSP</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Supply-side: publisher<br />inventory, builds bid<br />requests, resolves + stamps<br />audience segments<br />(consent-gated).</div>"]
      style 18 fill:#2563eb,stroke:#1e4fc2,color:#ffffff
      27["<div style='font-weight: bold'>Exchange</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Runs the auction, DSP<br />fan-out, deal priority<br />(PG>Preferred>PMP>Open),<br />win/loss notices (price +<br />clear_price).</div>"]
      style 27 fill:#ea580c,stroke:#c2490a,color:#ffffff
      36["<div style='font-weight: bold'>DSP</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Demand-side: campaign<br />eligibility, targeting,<br />bidding, budget/pacing, bid<br />shading.</div>"]
      style 36 fill:#7c3aed,stroke:#6530c4,color:#ffffff
      44["<div style='font-weight: bold'>Ad Server</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Creative decisioning +<br />serving, frequency capping,<br />HMAC-signed tracking macros.</div>"]
      style 44 fill:#16a34a,stroke:#12863d,color:#ffffff
      53["<div style='font-weight: bold'>Publisher Ad Server</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Direct-sold vs programmatic<br />arbitration per slot<br />(GAM-like) + Prebid fan-out +<br />guaranteed pacing.</div>"]
      style 53 fill:#0369a1,stroke:#025685,color:#ffffff
      62["<div style='font-weight: bold'>Tracker</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Impression/click/conversion/view<br />beacons, real-time fraud<br />checks, publishes events.</div>"]
      style 62 fill:#dc2626,stroke:#b61f1f,color:#ffffff
      71["<div style='font-weight: bold'>SSAI Stitcher</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Server-side ad insertion into<br />HLS/DASH manifests, signed<br />segment beacons.</div>"]
      style 71 fill:#db2777,stroke:#b52062,color:#ffffff
      79["<div style='font-weight: bold'>Transcoder</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Cache-first ad conditioning<br />(ffmpeg) to<br />content-compatible HLS for<br />SSAI.</div>"]
      style 79 fill:#be185d,stroke:#9c144c,color:#ffffff
      85["<div style='font-weight: bold'>Reporting + Billing</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Consumes NATS events →<br />ClickHouse; query API;<br />in-process billing engine<br />(reserve/settle,<br />TigerBeetle); hourly Parquet<br />export.</div>"]
      style 85 fill:#4f46e5,stroke:#413abd,color:#ffffff
      94["<div style='font-weight: bold'>Pipeline</div><div style='font-size: 70%; margin-top: 0px'>[Container: Go]</div><div style='font-size: 80%; margin-top:10px'>Publisher-file ingest,<br />audience membership<br />cache-writer (single writer),<br />batch-conductor chain.</div>"]
      style 94 fill:#0891b2,stroke:#067793,color:#ffffff
    end

    1-. "<div>Campaigns, audiences,<br />reports, bid-shading view<br />(HTTPS)</div><div style='font-size: 70%'></div>" .->10
    2-. "<div>Inventory, yield, payouts<br />(HTTPS)</div><div style='font-size: 70%'></div>" .->10
    3-. "<div>Ops, moderation, incidents<br />(HTTPS)</div><div style='font-size: 70%'></div>" .->10
    10-. "<div>SSO auth-code + PKCE</div><div style='font-size: 70%'></div>" .->7
    10-. "<div>Verification / notification<br />email</div><div style='font-size: 70%'></div>" .->8
    10-. "<div>Campaign CRUD (gRPC)</div><div style='font-size: 70%'></div>" .->36
    10-. "<div>Creative CRUD (gRPC)</div><div style='font-size: 70%'></div>" .->44
    10-. "<div>Inventory CRUD (gRPC)</div><div style='font-size: 70%'></div>" .->18
    10-. "<div>Report queries, trace,<br />shading (HTTP)</div><div style='font-size: 70%'></div>" .->85
    10-. "<div>Accounts, sessions, config<br />(RLS)</div><div style='font-size: 70%'></div>" .->145
    10-. "<div>Sessions, rate limits</div><div style='font-size: 70%'></div>" .->146
    4-. "<div>Ad request (web-mirror)</div><div style='font-size: 70%'></div>" .->18
    4-. "<div>Publisher ad request</div><div style='font-size: 70%'></div>" .->53
    53-. "<div>Programmatic demand</div><div style='font-size: 70%'></div>" .->18
    53-. "<div>Header-bidding fan-out<br />(OpenRTB HTTP)</div><div style='font-size: 70%'></div>" .->6
    18-. "<div>Run auction (gRPC twin)</div><div style='font-size: 70%'></div>" .->27
    27-. "<div>Bid request (gRPC twin)</div><div style='font-size: 70%'></div>" .->36
    27-. "<div>Bid request (OpenRTB HTTP)</div><div style='font-size: 70%'></div>" .->5
    36-. "<div>Bid (shaded)</div><div style='font-size: 70%'></div>" .->27
    27-. "<div>Win/loss notice — price +<br />clear_price (HTTP nurl)</div><div style='font-size: 70%'></div>" .->36
    27-. "<div>Winner → render creative</div><div style='font-size: 70%'></div>" .->44
    44-. "<div>Ad markup + signed beacon<br />URLs</div><div style='font-size: 70%'></div>" .->4
    4-. "<div>Impression / click / view<br />beacons</div><div style='font-size: 70%'></div>" .->62
    18-. "<div>Video/CTV stitch request</div><div style='font-size: 70%'></div>" .->71
    71-. "<div>Condition winning ad</div><div style='font-size: 70%'></div>" .->79
    62-. "<div>Impression/click/conversion/view<br />+ fraud</div><div style='font-size: 70%'></div>" .->148
    27-. "<div>AuctionWin / AuctionComplete<br />/ DSPCall</div><div style='font-size: 70%'></div>" .->148
    36-. "<div>AuctionLoss / AuctionShade /<br />BudgetDepleted</div><div style='font-size: 70%'></div>" .->148
    18-. "<div>Behaviour observed / data-fee</div><div style='font-size: 70%'></div>" .->148
    148-. "<div>Consumes all business events</div><div style='font-size: 70%'></div>" .->85
    148-. "<div>Account-scoped events</div><div style='font-size: 70%'></div>" .->125
    148-. "<div>Account-scoped events</div><div style='font-size: 70%'></div>" .->133
    148-. "<div>Observed identities</div><div style='font-size: 70%'></div>" .->100
    148-. "<div>Site-visit / purchase</div><div style='font-size: 70%'></div>" .->107
    36-. "<div>consumes cache-invalidate +<br />spend-snapshot</div><div style='font-size: 70%'></div>" .->148
    85-. "<div>Writes events; serves queries</div><div style='font-size: 70%'></div>" .->147
    85-. "<div>Ledger reserve/settle</div><div style='font-size: 70%'></div>" .->150
    85-. "<div>Invoices, balances, committed<br />spend</div><div style='font-size: 70%'></div>" .->145
    85-. "<div>Hourly Parquet export</div><div style='font-size: 70%'></div>" .->149
    85-. "<div>campaign_spend_snapshot<br />(pacing reconcile)</div><div style='font-size: 70%'></div>" .->148
    94-. "<div>Audience cache writer<br />(SADD/SREM)</div><div style='font-size: 70%'></div>" .->146
    94-. "<div>Ingested files / lake</div><div style='font-size: 70%'></div>" .->149
    94-. "<div>Normalised rows, memberships</div><div style='font-size: 70%'></div>" .->145
    137-. "<div>Drives rollups + Parquet<br />export</div><div style='font-size: 70%'></div>" .->85
    137-. "<div>Profile-builder / privacy<br />purge</div><div style='font-size: 70%'></div>" .->149
    100-. "<div>Identity graph edges</div><div style='font-size: 70%'></div>" .->145
    107-. "<div>audience_segment_members<br />(retarget)</div><div style='font-size: 70%'></div>" .->145
    115-. "<div>Report queries</div><div style='font-size: 70%'></div>" .->147
    115-. "<div>Report artifacts + export<br />zips</div><div style='font-size: 70%'></div>" .->149
    115-. "<div>Download links</div><div style='font-size: 70%'></div>" .->8
    36-. "<div>Budget/balance/audience reads</div><div style='font-size: 70%'></div>" .->146
    36-. "<div>Campaign warm-load<br />(cross-tenant loader)</div><div style='font-size: 70%'></div>" .->145
    18-. "<div>Audience sets (SMEMBERS)</div><div style='font-size: 70%'></div>" .->146
    18-. "<div>Placements, segments</div><div style='font-size: 70%'></div>" .->145
    44-. "<div>Frequency caps</div><div style='font-size: 70%'></div>" .->146
    44-. "<div>Creatives</div><div style='font-size: 70%'></div>" .->149
    18-. "<div>Render creative (gRPC twin)</div><div style='font-size: 70%'></div>" .->44
    27-. "<div>DSP endpoints, floors, deals,<br />ads.txt</div><div style='font-size: 70%'></div>" .->146
    44-. "<div>Recent SKUs + product catalog</div><div style='font-size: 70%'></div>" .->145
    44-. "<div>Cache-invalidate (creatives /<br />campaigns)</div><div style='font-size: 70%'></div>" .->148
    44-. "<div>Warm-start CTR histogram<br />(HTTP, boot)</div><div style='font-size: 70%'></div>" .->85
    53-. "<div>Warm-load publisher line<br />items</div><div style='font-size: 70%'></div>" .->145
    53-. "<div>Per-line-item delivery<br />actuals</div><div style='font-size: 70%'></div>" .->146
    53-. "<div>Render direct-sold creative<br />(HTTP)</div><div style='font-size: 70%'></div>" .->44
    53-. "<div>Publish serve/win events</div><div style='font-size: 70%'></div>" .->148
    62-. "<div>Poll fraud_blocklists</div><div style='font-size: 70%'></div>" .->145
    62-. "<div>SetNX dedup keys</div><div style='font-size: 70%'></div>" .->146
    62-. "<div>Event spool (NATS-stall<br />fallback)</div><div style='font-size: 70%'></div>" .->149
    94-. "<div>Ingest completion email</div><div style='font-size: 70%'></div>" .->8
    94-. "<div>Publish ProfileSignalEvent</div><div style='font-size: 70%'></div>" .->148
    100-. "<div>Consume identity.observed</div><div style='font-size: 70%'></div>" .->148
    100-. "<div>Fingerprint buckets (TTL)</div><div style='font-size: 70%'></div>" .->146
    107-. "<div>Consume behaviour.observed</div><div style='font-size: 70%'></div>" .->148
    115-. "<div>Load scheduled_reports</div><div style='font-size: 70%'></div>" .->145
    115-. "<div>Query API (HTTP)</div><div style='font-size: 70%'></div>" .->85
    71-. "<div>Run per-break auction (HTTP)</div><div style='font-size: 70%'></div>" .->18
    71-. "<div>Read conditioned ad cache</div><div style='font-size: 70%'></div>" .->149
    71-. "<div>RECORD freq cap (HTTP)</div><div style='font-size: 70%'></div>" .->44
    71-. "<div>Fire signed<br />impression/quartile beacons</div><div style='font-size: 70%'></div>" .->62
    79-. "<div>Exists lookup</div><div style='font-size: 70%'></div>" .->149
    125-. "<div>Consume account-scoped events</div><div style='font-size: 70%'></div>" .->148
    125-. "<div>Read webhooks (RLS)</div><div style='font-size: 70%'></div>" .->145
    133-. "<div>Consume account-scoped events</div><div style='font-size: 70%'></div>" .->148
    133-. "<div>Insert notifications (RLS)</div><div style='font-size: 70%'></div>" .->145
    10-. "<div>Read bell:<br />list/unread/mark-read</div><div style='font-size: 70%'></div>" .->133
    137-. "<div>Probe /readyz</div><div style='font-size: 70%'></div>" .->94
    137-. "<div>Write audience memberships</div><div style='font-size: 70%'></div>" .->145
    137-. "<div>Behaviour signal reads</div><div style='font-size: 70%'></div>" .->147
    137-. "<div>run_completed announcement</div><div style='font-size: 70%'></div>" .->148

  end
```

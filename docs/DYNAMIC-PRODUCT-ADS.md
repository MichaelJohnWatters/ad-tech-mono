# Dynamic Product Ads (DPA)

Catalog-driven retargeting: the chase ad shows the shopper's **actual carted
product** (not a static banner), and on purchase it stops featuring what they
bought and rotates to a cross-sell complement. Built on the existing retargeting
chase (see `docs/demos/RETARGETING-CHASE-DEMO.md`) — DPA changes the *what* (the
creative content), not the *who* (the audience targeting).

Shipped 2026-08-09 in four independently-demoable slices.

## The four moving parts

### 1. Product catalog (`products` table, migration 083)
An advertiser-scoped product feed: `sku`, `title`, `image_url`, `price_micros`
(micro-dollars), `availability`, `product_url`, `category`, `complement_sku`.
The feed rides the **existing unified ingestion path** (ADR 0007/0008) as a
second feed *type* — same `audience_ingest_jobs` queue with `kind='product'`,
same staging / inline-vs-202 / strict all-or-nothing validation / PGP as an
audience upload. It just lands in `products` instead of segment memberships.

- API: `GET/POST /v1/api/products` (gateway) — reuses the audience upload deps.
- Feed columns accept Google-Merchant aliases (`id`/`item_id`, `image_link`,
  `link`, `google_product_category`) so a standard feed works as-is.
- Portal: advertiser **Products** tab (catalog table + feed upload).
- Code: `pkg/catalog`, `pkg/catalog/postgres`, `pkg/ingest/products.go`.

### 2. SKU-aware retargeting pixel (`retargeting_product_views`, migration 084)
`/v1/t/rt` gains `sku=` / `skus=` (CSV): the SKUs a shopper viewed/carted.
audience-rt records them per-user (**person + household**, like enrollment) with
the same TTL semantics as membership — the memory a dynamic creative renders
from. Independent of segment matching: a product-page pixel builds SKU memory
even before the shopper enrolls.

- SDK: `adtechadv.setProductSKUs([...])` → pixel carries `skus=`.
- Config: `audience_rt.product_view_days` (TierLive, 30d).
- Code: `pkg/audience/store/postgres/product_views.go`, `pkg/retargeting`.

### 3. Dynamic creative assembled at render (`dynamic_product` format, mig 085)
A creative with `format=dynamic_product` carries a **Go template** in
`html_content`. At serve time the ad server reads the user's recent SKUs +
looks them up in the catalog and executes the template with those products; the
template's `{{else}}` branch is the **static fallback** for a visitor with no
SKU context (fresh, no-consent, or stores down). Ad macros (`${CLICK_URL}` etc.)
survive template execution and are substituted afterward, so click/beacon URLs
work in either branch.

```
<div>{{range .Products}}
  <a href="${CLICK_URL}"><img src="{{.ImageURL}}">{{.Title}} {{.PriceDisplay}}</a>
{{else}}
  <a href="${CLICK_URL}">Shop our range</a>   {{/* static fallback */}}
{{end}}</div>
```

- Assembly is post-auction (once per won impression), bounded + 100ms-timeout,
  fail-open to the static branch — a dynamic creative never renders worse than
  static. Not on the bid loop.
- `dynamic_product` is display-family: eligible for a display request in the
  DSP's creative selection.
- Code: `cmd/adserver/dynamic_products.go`; gateway validates the template at
  upload.

### 4. Per-product suppression + cross-sell (migration 086)
A **SKU-carrying purchase** (`/v1/t/conv?skus=…`) is per-product, not
whole-person:
- **Suppress the bought SKU** — removed from `retargeting_product_views` AND
  durably burn-listed (`retargeting_suppressions` gains a `sku` dimension:
  `sku=''` is the whole-person burn, a value is per-product). The record path
  filters burned SKUs so a stale pixel can't re-add a bought product.
- **Cross-sell** — the bought product's `complement_sku` is added to the views,
  so the creative rotates to the complement (bought the kibble → show the
  treats).
- **Keep chasing** the rest of the cart. Only when the cart is fully cleared
  (nothing left to feature) does it fall through to **whole-person suppression**
  — the pre-DPA behaviour, unchanged for generic (no-SKU) conversions.

- Code: `pkg/retargeting.Service.OnConversion(…, boughtSKUs)`,
  `pkg/audience/store/postgres/suppressions.go`.

## Ops / gotchas

- **RLS empty-string cast (migration 087, latent bug found live):** the DPA
  tables' policies cast `current_setting('app.current_account_id', true)::UUID`
  directly, which raises `22P02` on the **empty string** — a pooled connection
  that ran a tenant tx leaves the GUC `''` (not unset), so the platform-hatch
  reads on the conversion path errored. `NULLIF(..,'')` makes the cast
  null-safe. Any new platform-hatch policy of this shape needs the same guard.
  (The adserver render read never hit it — its pool runs no tenant writes.)
- **SDK served by the gateway:** `adtech-adv.js` edits need
  `make deploy SVC=gateway`.
- **Demo:** run demoadv as barkbox with `DEMOADV_SKU=DOG-KIBBLE-12KG` (default);
  the seeded barkbox catalog has cross-sell pairs (kibble↔treats). The chase
  creative `cr-dogfood-cart-001` is now `dynamic_product`.

## Tests

- e2e: `TestProductCatalogFeedIngest`, `TestProductSKUPixelRecordsViews`,
  `TestDynamicProductCreativeAssembly`, `TestPerProductSuppressionAndCrossSell`.
- Existing whole-person suppression e2e (realtime / durable / household) stay
  green — generic conversions are unchanged.

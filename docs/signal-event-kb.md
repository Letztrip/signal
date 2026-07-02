# Signal — Event Tracking Knowledge Base

Cross-platform reference for the **signal** analytics system: every event, property
convention, and per-screen call site across **web (orbitra)** and **Flutter
(letztrip-frontend)**, plus the collector rules and the gotchas that bite.

Source of truth = the call sites in each repo (this doc is generated from a sweep +
the collector schema; re-grep `track(` when in doubt).

---

## 1. How it works (pipeline)

```
app (orbitra / letztrip-frontend)
  → track(event_name, properties)        client SDK batches + flushes
  → POST https://api-signal.travafa.com/v1/events   (X-Write-Key header)
  → signal collector (Go)                validate + enrich + PII scan
  → Pub/Sub topic events-raw
  → BigQuery  analytics.events            (native subscription)
  → pulse-service  /api/v1/signal/*       reads/aggregates (funnels, analytics)
  → pd dashboard (Signal UI)
```

- **Collector**: `D:\Letztrip\signal\collector` (auth.go = write-key + CORS, server.go = ingest, pii.go, enrich.go).
- **Web SDK**: `orbitra/src/core/tracking/track.ts` (helper) + `app/AnalyticsBoot.jsx` (auto page_viewed + session_started).
- **Flutter SDK**: `letztrip-frontend/lib/track.dart` + `TrackNavigatorObserver` in `lib/go_router_config.dart`.
- **Reader**: `pulse-service/src/main/java/com/letztrip/pulse/signal/*`.

---

## 2. HARD RULES & GOTCHAS (read before instrumenting)

1. **`event_name` is a fixed 7-value enum** (`collector/schemas/events.v1.json`):
   `page_viewed, button_clicked, form_submitted, identify, session_started, error_occurred, scroll_depth`.
   **Anything else → HTTP 400 `schema_violation` → the whole batch is dropped.**
   Put the semantic name in `properties.id` (actions) or `properties.name` (screens),
   NOT in `event_name`. ⚠️ Many web events today use custom names and are silently
   dropped (see §4.5).
2. **Property conventions** (how the dashboard/funnels read events):
   - `page_viewed` → `properties.name` = path (web) or screen name (Flutter).
   - `button_clicked` / `error_occurred` → `properties.id` = dotted action id (e.g. `flight.booking.continue`).
   - `form_submitted` → `properties.id` = form id (e.g. `flight_search`, `auth.login`).
3. **Web paths have trailing slashes** — orbitra runs Next.js `trailingSlash:true`, so
   `page_viewed.name` = `/flight/search/` (not `/flight/search`). pulse funnel RTRIMs
   trailing slashes so either matches.
4. **Web and Flutter name the same stage DIFFERENTLY** — web uses URL paths, Flutter uses
   GoRouter route names + form/button ids. There is **no shared value** per stage. Cross-
   platform funnels must OR both (pulse `Step.anyOf`); see §6.
5. **Sessionization differs**: collector session = 30-min rollover; web SDK session id is
   **per-tab** (sessionStorage `x-session-id`); Flutter manages its own. pulse funnels +
   retention + stickiness sessionize by **`anonymous_id`** (device), NOT session_id.
6. **Auth = write key, requests must be non-credentialed.** CORS is `Access-Control-Allow-Origin: *`
   → clients must send `credentials:'omit'` and must NOT use `navigator.sendBeacon` (beacons
   are credentialed + can't set `X-Write-Key`). Web `track.ts` uses keepalive `fetch`.
7. **`app_id`** is injected server-side from the write key (observed `travafa`), NOT sent by
   the SDKs. There is no platform/app filter on the funnel endpoint.
8. **`identify`**: Flutter fires a real `identify` event; web uses `setUserId()` (no `identify`
   event emitted). User attribution on web comes from `user_id` on subsequent events.

---

## 3. Event model (BigQuery `analytics.events`, 17 columns)

`event_id, event_name, user_id, anonymous_id, session_id, client_ts, server_ts,
platform, app_version, sdk_version, app_id, ua_family, ua_os, geo_country,
ingest_version, properties (JSON), context (JSON)`.

- `platform` ∈ `web | flutter_ios | flutter_android`.
- `context` carries (web) `page.{path,referrer,title}`, `screen`, `locale`, `timezone`;
  (Flutter) `platform`, `app_version`, `sdk_version` + device.
- `properties` = the per-event payload (`name` / `id` + extras).

---

## 4. WEB — orbitra (`event_name` + `properties`)

Web SDK: `src/core/tracking/track.ts`. Auto events from `app/AnalyticsBoot.jsx`.

### 4.1 page_viewed  (auto, every route)
`properties.name` = `usePathname()` value, **with trailing slash**. `properties.query` = search params.
Key flow paths:
| Flow | paths (`name`) |
|---|---|
| Flights | `/flight/search/`, `/flight` (results), `/flight/booking/` |
| Hotels | `/hotel/search/`, `/hotel/details/<id>/`, `/hotel/booking/` |
| Checkout/convert | `/checkout/`, `/payment-success/`, `/hotel-checkout/`, `/hotel-payment-success/`, `/booking-failed/` |
| Other | `/`, `/feed`, `/discussion`, `/trips`, profile/dashboard routes |

### 4.2 session_started  (auto)
Fired once per session id (guarded in sessionStorage `signal.session_started_for`). No properties.

### 4.3 button_clicked — `properties.id` catalog
| Area | ids |
|---|---|
| Header/nav | `header.logo`, `header.mobile_menu_open`, `header.nav.feed`, `header.nav.discussion`, `header.nav.trips`, `header.nav.friends`, `header.login`, `header.profile.logout`, `header.profile.toggle`, `mobile_menu.nav`, `mobile_menu.logout`, `mobile_menu.login`, `bottom_nav.tab` |
| Footer | `footer.social`, `footer.menu_login`, `footer.menu_link`, `footer.app_store`, `footer.play_store` |
| Hero | `hero.ai_trip_banner`, `hero.popular`, `hero.tab`, `hero.group_trip` |
| Auth | `auth.signup.go_login`, `auth.login.go_signup`, `auth.login.forgot_password`, `auth.mobile_login.google`, `auth.mobile_login.email`, `auth.social.google`, `auth.social.mobile`, `auth.otp.resend/verify/change_number`, `auth.email_otp.resend/verify/change_email`, `auth.forgot_password.resend/change_email/back_to_login` |
| Flights | `flight.booking.continue`, `flight.prebook.started`, `flight.booking.price_change.continue`, `flight.result.book_now`, `flight.result.view_details` |
| Checkout/bookings | `checkout.pay_now`, `checkout.price_change.ok`, `checkout.price_change.go_back`, `booking.cancellation.confirm`, `booking.download_ticket`, `booking.refund.request` |
| Hotels | `hotel.prebook.started`, `hotel.checkout.pay_now`, `hotel.result.see_availability`, `hotel.booking.cancel` |
| Activities | `activity.result.book_now` |
| Profile/dashboard | `my_bookings.row.open`, `my_bookings.tab`, `settings.reset`, `settings.delete_account.open`, `subscription.plan.select`, `feedback.cancel`, `feedback.submit` |
| CTA/feed | `cta.start_exploring`, `banner.download_app`, `feed.open_user_profile` |
Common extra props: `booking_id`, `href`, `tab`, `plan`, `destination`, `total_price`.

### 4.4 form_submitted — `properties.id`
`auth.signup`, `auth.login`, `auth.mobile_login.send_otp`, `auth.forgot_password.request`, `auth.forgot_password.confirm`.

### 4.5 ⚠️ error_occurred
`id: "flight.booking"` (+ `reason`) on flight payment failure (`app/(bookings)/checkout/CheckoutClient.jsx`).

### 4.6 ⚠️ WEB CUSTOM EVENTS THAT ARE **DROPPED** (violate the 7-enum)
These call sites use a non-enum `event_name` → rejected by the collector, never reach BigQuery:
`flight_search`, `hotel_search`, `activity_search`, `article_search`, `feedback_submitted`,
`settings_saved`, `subscription_subscribe_clicked`, `user_signed_up`, `user_signed_in`,
`password_reset_completed`, `account_deleted`, `coupon_applied`, `coupon_apply_failed`,
`coupon_removed`, `comment_added`, `comment_deleted`, `friend_invited`, `discussion_created`.
**Fix pattern**: re-emit as `form_submitted`/`button_clicked` with the name in `properties.id`
(e.g. `track("form_submitted", { id: "flight_search", ... })`).

---

## 5. FLUTTER — letztrip-frontend (`lib/track.dart`)

`page_viewed` auto-fired by `TrackNavigatorObserver` (GoRouter `name:` → `properties.name`).
Flutter is mostly enum-compliant (uses form_submitted/button_clicked/page_viewed with ids/names).

### 5.1 page_viewed — route `name` values (`go_router_config.dart`)
Flights: `flights` (results), `filter`, `review_booking`, `review_feature_selection`,
`traveller_form`, `traveller_review`, `payment_summary`, `confirm_booking`,
`flight_booking_details`, `empty_flight_screen`, `fare_update`.
Stays: `stays`, `stay_booking_detail`, `room_selection`, `stay_review_booking`, `travel_info`,
`hotel_payment_summary`, `hotel_facility`, `stay_confirm_booking`, `user_stay_booking_detail`.
Activities: `activity_details`, `activity_review_booking`, `activity_confirm_booking`.
Other: `login`, `verify-otp`, `notifications`, `session_expired`, `payment_failed`,
`booking_pending`, `load_payment`, `contact_us`, `explore_booking_history`.
Unnamed routes emit the **path** as name: `/`, `/home`, `/home/book` (the combined
search hub — flights+stays+experiences tabs, one screen), `/onboarding`, `/create-profile`, etc.

### 5.2 form_submitted — `properties.id`
`flight_search` (+ trip_type/from/to), `stay_search` (+ city), `verify_traveller_details`
(+ is_hotel), `flight_cancel` (+ booking_id, is_modify), `verify_otp`, `email_verify`,
`create_profile`, `add_preferences`, `update_preferences`, `forgot_password.confirm`,
`edit_profile`, `edit_profile.social_link`, `feedback`, `search.request_location`,
`ai_suggestions.prompt_generate`, `location.activity_suggestion`.

### 5.3 button_clicked — `properties.id`
`flights_list.continue`, `flight_payment.continue` (+ booking_id), `room_selection.continue`
(+ search_request_id), `stay_review.continue` (+ search_request_id), `stay_payment.continue`
(+ option), `bookings.tab`.

### 5.4 other
`session_started` (main.dart), `identify` (on sign-in, new_home.dart), `scroll_depth`
(opt-in, `percent`+`name`). ⚠️ `subscription_purchased` is custom → **dropped** (enum).

### 5.5 Ordered funnels (Flutter)
**Flights**: `pv /home/book` → `form_submitted flight_search` → `pv flights` →
`button_clicked flights_list.continue` → `pv review_booking` → `pv review_feature_selection`
→ `pv traveller_form` → `form_submitted verify_traveller_details` → `pv traveller_review` →
`pv payment_summary` → `button_clicked flight_payment.continue` → `pv confirm_booking`.
**Stays**: `pv /home/book` → `form_submitted stay_search` → `pv stays` → `pv stay_booking_detail`
→ `pv room_selection` → `button_clicked room_selection.continue` → `pv stay_review_booking` →
`button_clicked stay_review.continue` → (`pv travel_info`, conditional) → `pv hotel_payment_summary`
→ `button_clicked stay_payment.continue` → `pv stay_confirm_booking`.
Note: confirmation screens have **no** track() — only the `page_viewed` fires.

---

## 6. CROSS-PLATFORM FUNNEL MAPPING (web ↔ Flutter)

Used by pd presets (`pd-frontend/src/pages/signal/funnelPresets.js`) via pulse `Step.anyOf`
(a step matches an event satisfying ANY predicate).

### Flights
| Stage | Web | Flutter |
|---|---|---|
| Search | `page_viewed /flight/search/` | `form_submitted flight_search` |
| Results | `page_viewed /flight` | `page_viewed flights` |
| Selected | `button_clicked flight.booking.continue` | `button_clicked flights_list.continue` |
| Review | `page_viewed /flight/booking/` | `page_viewed review_booking` |
| Payment | `page_viewed /checkout/` | `page_viewed payment_summary` |
| Pay clicked | `button_clicked checkout.pay_now` | `button_clicked flight_payment.continue` |
| Converted | `page_viewed /payment-success/` | `page_viewed confirm_booking` |

### Stays
| Stage | Web | Flutter |
|---|---|---|
| Search | `page_viewed /hotel/search/` | `form_submitted stay_search` |
| Results | `page_viewed /hotel/search/` | `page_viewed stays` |
| Detail | (dynamic path — unmatched) | `page_viewed stay_booking_detail` |
| Room selected | — | `button_clicked room_selection.continue` |
| Review | `page_viewed /hotel/booking/` | `page_viewed stay_review_booking` |
| Payment | `page_viewed /hotel-checkout/` | `page_viewed hotel_payment_summary` |
| Converted | `page_viewed /hotel-payment-success/` | `page_viewed stay_confirm_booking` |

---

## 7. Querying / building funnels

- Funnel endpoint: `POST /api/v1/signal/funnels/compute` — body `{from, to (ISO instants),
  steps:[{anyOf:[{eventName, propertyFilters:{key:value}}]}]}`. Sessionized by `anonymous_id`,
  results split by **entry platform** (web/ios/android) for the device-segmented view.
- Other reads: `/events/recent`, `/events/by-name`, `/metrics/{active-users,stickiness,
  session-quality,bounce-rate}`, `/pages/{views,scroll-depth}`, `/retention/cohort`,
  `/sessions/{id}/*`, `/users|anonymous/{id}/*`. (See pulse `signal/api/*`.)
- `propertyFilters` matches `JSON_VALUE(properties,'$.<key>')` (equality, trailing-slash-insensitive).

---

## 8. Recommended cleanup (tracked debt)
1. **Web dropped events (§4.5)** — migrate each custom name to `form_submitted`/`button_clicked`
   + `properties.id`. Biggest gap: `flight_search`/`hotel_search` (search intent invisible on web).
2. **Canonical screen ids** — long-term, have web + Flutter emit a shared `properties.id`/`screen`
   per funnel stage so funnels don't need per-platform `anyOf`.
3. **Flutter `subscription_purchased`** — rename to an enum event.
4. **Confirmation events** — Flutter booking-confirm screens emit only `page_viewed`; add an
   explicit `button_clicked`/`form_submitted` for booked if a precise conversion signal is needed.

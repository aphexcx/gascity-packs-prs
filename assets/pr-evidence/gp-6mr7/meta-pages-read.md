# Meta pages read for whatsapp/docs/meta-setup.md (gp-6mr7)

Read 2026-11-04 19:39Z by curl of each page's Markdown form (`<page>.md`,
the form Meta's docs index `llms.txt` lists). The old `/docs/whatsapp/...`
URLs redirect to `/documentation/business-messaging/whatsapp/...`; every
citation on the page names the new URL. The first 16 hex of each fetched
file's SHA-256 identify the copy read.

Base: `https://developers.facebook.com/documentation/business-messaging/whatsapp/`

| Page (under the base unless absolute) | sha256[:16] | What the setup page takes from it (quoted) |
| --- | --- | --- |
| `get-started` | 397b47926f2acab7 | Step 1: "Select the **Connect with customers through WhatsApp** use case and click **Next**." "Select an existing business portfolio or create a new one." "you are redirected to the **Customize use case > Connect on WhatsApp > Quickstart** page". Step 2: "Click the **Start using the API** button ... You are redirected to the **API Setup** page." "**To create a new account:** Click **Create a WhatsApp Business account**". Step 3: "Select a **From** phone number ... Add a **To** phone number" and "retain both your test phone number ID and Messaging account ID". Step 5: "Navigate to Business Settings and click **System users** in the sidebar." "Click the **Add+** button" "click **Assign Assets.**" "toggle **Manage app** under **Full control.**" "toggle **Manage WhatsApp Business accounts** under **Full control.**" "Click **Generate token.**" with `business_management`, `whatsapp_business_messaging`, `whatsapp_business_management`. |
| `about-the-platform` | 1d6d8b7bd55206c3 | "**WhatsApp account (WAAC)** — ... Contains your phone numbers, display name" and "**Messaging account** — Manages your message templates, billing, and payment methods." Test resources: test numbers "have relaxed messaging limits and don't require a payment method on file in order to send template messages." The recipient cap of five phones the design read on 11/3 is stated on none of the pages read today. |
| `webhooks/create-webhook-endpoint` | 5e8ca0d7dd4bc41e | "navigate to the **App Dashboard** > **WhatsApp** > **Configuration** panel, and add your webhook endpoint URL to the **Callback URL** field, and your verification string to **Verify token** field." "if you created your app using the **Connect with customers through WhatsApp** use case, navigate to **App Dashboard** > **Use cases** > **Customize** > **Configuration** instead." `hub.verify_token`: "A verification string of your own choosing." |
| `webhooks/overview` | c05e0735f3ba6870 | Subscribe to fields in the Configuration panel; `messages`. Troubleshooting: "Make sure your app is in **Live** mode; some webhooks will not be sent if your app is in **Dev** mode." |
| `business-phone-numbers/phone-numbers` | 48b5204ccd6ce9c7 | "Numbers already in use with WhatsApp cannot be registered unless they are deleted first." A number must "be able to receive voice calls or SMS". Add via "**App Dashboard** > **WhatsApp** > **API Setup**" or WhatsApp Manager; status in "**WhatsApp Manager** > **Account tools** > **Phone numbers**". |
| `business-phone-numbers/registration` | 33f39f8726377007 | "You can only register a number via the API — you cannot register a number through WhatsApp Manager (WAM) or the App Dashboard." `POST PHONE_NUMBER_ID/register` with `messaging_product` and the 6-digit `pin`; example on `graph.facebook.com/v26.0`. |
| `display-names` | 63b61d73925d4547 | The display name shows at the top of chats when "approved via display name verification"; the Name column in "**WhatsApp Manager** > **Account tools** > **Phone numbers**". |
| `business-profiles` | 43ffaaf44d4f9344 | Update the profile at "**WhatsApp Manager** > **Account tools** > **Phone numbers**". |
| `templates/overview` | 636489f74cb2267c | Create a template in the "message templates panel" in WhatsApp Manager (`business.facebook.com/latest/whatsapp_manager/message_templates`); positional parameters `{{1}}`. |
| `templates/template-review` | 4ad0d5974ea040a2 | "It can take up to 24 hours for an approval decision". Common rejection reasons: "The message template cannot start or end with a parameter (dangling parameters are not allowed)." and "too many variable parameters relative to the message length". Samples are required for variables. |
| `templates/template-categorization` | 2af970d92798fd95 | "If you selected `UTILITY` as the template's category and WhatsApp determined it should be `MARKETING`, **the template is approved as `MARKETING`**", and "a recurring process to identify and update approved templates that should be of a different category"; the setup page says only that Meta may file `dynasty_update` as marketing. |
| `pricing` | 0a5d1a970c11e6e5 | Free tier: "1,000 delivered service messages per month" per number; "If businesses do not have a payment method for their Messaging account(s), Meta will deliver service messages within the free tier but _not_ deliver them after the free tier has been used." Billing Hub `business.facebook.com/latest/billing_hub/`. |
| `pricing/ai-providers` | ac8582251c8ca810 | Terms of 2026-01-15: "'AI Providers' are only permitted to offer general-purpose AI assistants on the WhatsApp Business Platform where Meta is legally required to permit this use case." |
| `getting-opt-in` | df17e5780907aa89 | "Businesses are required to obtain opt-in before messaging people on WhatsApp." |
| `access-tokens` | 33c1ed99e314ea57 | "By default, admin system users have full access to all WhatsApp accounts (WAACs), Messaging accounts, and their assets". Employee system users: "you must grant **Partial** or **Full** business asset access for **both** accounts ... the **WhatsApp account** and the **Messaging account**"; "**Meta Business Suite.** The panel does not cascade." A missing grant is rejected "with error code `200`". Generate token: "select your app, choose a token expiration preference", the three permissions. (Read after the self-review's M1.) |
| `whatsapp-business-accounts` | 2ca3183c0f283f39 | "The **API Setup** panel allows you to add a production business phone number. This generates a WhatsApp account containing that number, along with a Messaging account for your templates and billing." (Read after the self-review's MINOR 4: the template must be made again on the real line's Messaging account.) |
| `templates/marketing-templates/per-user-limits` | 0add1eb271296926 | "WhatsApp does not currently deliver marketing template messages to WhatsApp users with United States phone numbers (numbers composed of a +1 dialing code and a US area code)." (Read 21:0xZ after codex QUICK r1's P1: a Marketing approval of `dynasty_update` is a blocker for the US owners.) |
| `support/error-codes` | d26b62f9b41cc8f1 | `131050`: "Unable to deliver the message. This recipient has chosen to stop receiving marketing messages on WhatsApp from your business." (Read after the self-review's MINOR 3.) |
| `https://developers.facebook.com/documentation/development/create-an-app/app-dashboard/basic-settings` | 1bdd055a1340ecea | "The Basic settings page ... find the app secret assigned to your app by Meta." |
| `https://developers.facebook.com/documentation/development/create-an-app/whatsapp-use-case` | 8c73cb284b3a4032 | Publish: "select **Publish** in the left side menu", "**Review** your use cases", "Click **Publish**"; required assets: an app icon, a privacy policy URL, a data deletion URL. |

## What the reading changed against the design

1. **The template body.** The design's `Your city has an update: {{1}}`
   ends with its parameter, which the template review page lists as a
   rejection reason. The setup page gives each language a fixed closing
   line (`Reply here to continue.` / `Responda aqui para continuar.`). The
   adapter sends only the name, the language and the parameter, so no code
   changes; bead A's `whatsapp/commands/publish/help.md` and the gateway's
   README (gp-nn90) still quote the body without the closing line.
2. **Registration is an API call.** A real number cannot be registered in
   either dashboard; the setup page names the one `register` call.
3. **A payment method matters for replies too.** Since 2026-10-01 a number
   with no payment method stops delivering service messages after its 1,000
   free ones each month.
4. **Live mode.** Publishing the app asks for an app icon, a privacy policy
   URL and a data deletion URL, which Dynasty must have before step 8.
5. **The test number's cap** (five phones in the design) is not stated on
   today's pages; the setup page says so instead of repeating it.
6. **The token needs the Messaging account too.** An Admin system user has
   it by default; an Employee needs both accounts assigned by hand.
7. **The real line brings its own Messaging account**, so `dynasty_update`
   is made again there and approved before the gateway switches numbers.
8. **The display name** is checked only when the number reaches a higher
   messaging limit; until then the owner sees the contact name they saved.

## Round 2 (gp-awtb, 2026-11-04 22:55Z): the pages the done/waiting rewrite cites

Fetched 22:55Z the same way (the `.md` form under the base; the two
`/docs/` pages have no `.md` form, so their HTML was read and its tags
stripped). Every quote below was matched in the fetched copy after
stripping Markdown links, backticks and bold.

| Page | sha256[:16] | What the setup page takes from it (quoted) |
| --- | --- | --- |
| `messaging-limits` | 4fdffd2516ee044d | "Newly created business portfolios have a messaging limit of 250"; business verification is one of the paths to 2,000 (step 10). |
| `business-phone-numbers/phone-numbers` | 2507b2973bf57cce | "New business portfolios are initially capped at two registered business phone numbers"; on verification "Meta will automatically increase your cap to 20" (step 10). |
| `display-names` | a7b9cee65c9b277d | "When you reach a higher messaging limit, your business phone number's display name automatically undergoes verification" (step 10). |
| `templates/template-categorization` | 2af970d92798fd95 | The review: "Go to Business Support", "Template Category Updates", "Request Review", "up to 60 days from the date the category was updated"; utility: "non-promotional" and "specific to or requested by the user (clearly related to their order, account, services, or transactions)" (step 9). Same copy as round 1. |
| `templates/template-management` | 304b55aba384afd9 | "Only templates with an APPROVED, REJECTED, or PAUSED status can be edited."; "up to 10 times in a 30-day window, or 1 time in a 24-hour window"; "If you delete an approved template, you cannot create a new template with the same name for 30 days" (step 9). |
| `templates/marketing-templates/per-user-limits` | 0add1eb271296926 | Error `131049` for the per-user marketing limit (step 9, the skill). Same copy as round 1. |
| `webhooks/create-webhook-endpoint` | 5e8ca0d7dd4bc41e | The Application Subscriptions API as the alternative to the panel, with an app token and `whatsapp_business_account` as the object; "Anytime you set or edit the Callback URL field or the Verify token field in the App Dashboard, Meta sends a GET request" (step 7). Same copy as round 1. |
| `solution-providers/manage-webhooks` | c3598a378798245d | "You must individually subscribe to every Messaging account for which you want to receive webhooks."; `POST /<WABA_ID>/subscribed_apps` (step 7). |
| `https://developers.facebook.com/docs/graph-api/reference/application/subscriptions/` (HTML) | 84a516ae70e9cebb | "An app access token is required to add new subscriptions for that app."; under Limitations "Webhooks for WhatsApp is not supported": the conflict step 7 names, with the panel as the fallback. |
| `https://developers.facebook.com/docs/development/release/business-verification` (HTML) | 80f932a10fb9c3e0 | The status path "Settings > Basic > Verification" (step 10). |

Not read: Meta's Help Center articles on verification documents and display
name guidelines (www.facebook.com/business/help/..., which answer a fetch
with the title only). No Meta page read here names a DBA; the page states
the DBA as Taylor's plan, not as Meta's rule.

### The facts of the day and where each comes from

| On the page | Source (city records, read by the lane) |
| --- | --- |
| The Houdini portfolio; the app 'Dynasty Computer'; the Messaging account (the id the API used as `<WABA_ID>`); the test number and its phone number id; the system user dynastywhatsapp (round 3: every id taken off this map and the page, the repo being public; the ids stay in the sources at right and in `~/.config/citadel-mayor/whatsapp/meta-ids.env`) | the 498th session file, 20:46Z to 21:07Z and 20:59Z bullets (assets/handoff/sessions/2026-11-04-498th.md); the bead gp-awtb |
| The two account names (round 3, the read r2's NOTE 3): the Messaging account "Test Messaging Account"; the WhatsApp account "Test WhatsApp Account" | the bead gp-awtb's facts ("('Test Messaging Account')" beside the account the API used); the 498th session file, 20:59Z bullet (Taylor 20:56:28Z: "the Test WhatsApp Account its one asset") |
| No second portfolio | the 498th session file, 20:39Z bullet (Taylor 20:35:46Z) |
| The gateway live on workers.dev; the five secrets; the custom-domain switch | assets/ops/records/whatsapp-gateway-deploy-20261104/README.md |
| The WABA subscribed to the app (21:17Z); the webhook set by `POST /<APP_ID>/subscriptions` with the app token (21:23Z), validated, active | the 498th session file, 21:17Z and 21:23Z bullets |
| The template filed by the API 21:17Z as UTILITY, en_US and pt_BR (one template id each, in the source at right), PENDING | the 498th session file, 21:17Z bullet |
| Meta classed both MARKETING; the verification status pending (the Graph reads 21:5xZ) | the bead gp-awtb (the 499th mayor's reads) |
| The test message (21:35Z); the payment method | assets/records/founder-words/taylor-20261104-213516Z-whatsapp-test-sent-handoff.md |
| The business verification submitted (21:30Z); the DBA | assets/records/founder-words/taylor-20261104-213054Z-business-verification-dba-dynasty-computer.md |
| The two Live-mode URLs | houmanoids/dynasty PR 20, merged 22:45:04Z (gh); `curl` 22:47:49Z: both 200, titles 'Privacy Policy \| Dynasty' and 'Data Deletion \| Dynasty' |
| The icon sizes (48 and 180 px PNG; `app/icon.svg`) | houmanoids/dynasty main, `sips` on `app/icon.png` and `app/apple-icon.png` |

The skill's failed-delivery line: a `status` delivery exists only from
Meta's status webhook (whatsapp/gateway/src/gateway.ts:612-632 on main at
54f53be7, the row routed to the city whose send recorded the wamid, with the
send's recipient as `wa_id` and `profile_name` NULL, also at :815-816); the
adapter renders a failed one at whatsapp/adapter/src/render.js:80-83 and
posts it under that `wa_id`'s actor (:123), whose display name falls back to
the bare `wa_id` (codex QUICK r2's P3), and only `message` and `joined` deliveries move the window
(inbound.js:293-298, the test at :296). A refusal at send time is the publish verb's error
instead (gateway.ts:790-805).

## Round 3 (gp-28td, 2026-11-04 23:41Z to 23:42Z): the cutover's subscription and the DBA clause

Fetched by curl into a fresh scratch directory. The Help Center article
answers with its title in the HTML, but its body is in the page's script
data, every `<` JSON-escaped; unescaped and its tags stripped, the section
text below was read. The raw page differs fetch to fetch, so the
hash of the extracted section text is given beside it.

| Page | sha256[:16] | What the setup page takes from it (quoted) |
| --- | --- | --- |
| `solution-providers/manage-webhooks` | 9ffae682743246e3 | "You must individually subscribe to every Messaging account for which you want to receive webhooks."; `POST /<WABA_ID>/subscribed_apps` "to subscribe your app to webhooks on the business customer's Messaging account"; with several accounts subscribed, Meta sends "all webhook notifications to the app's callback URL"; `GET /<WABA_ID>/subscribed_apps` lists the subscribed apps; the page names no dashboard view of the subscription (steps 3, 7 and 13). |
| `display-names` | cbc2ea5a580ea382 | "See our Display name guidelines for the WhatsApp Business Platform Help Center article for naming guidelines." (the link to the article below; step 10). |
| The Help Center article `display-names` links as "Display name guidelines for the WhatsApp Business Platform" (HTML; fetched at that link with `?locale=en_US&_fb_noscript=1`; its number is left in the link, out of this file, so the no-id check reads it clean) | raw 2b4fbe84d2ea9ec3; the section "Legal name relationship and branding" as text 76ae9882f9169982 | "A WhatsApp Business Platform display name must have a clear relationship with the business's legal name." The article names no DBA (no "DBA", "doing business", "trade name" or "registered" in its text, 8,143 characters from its first heading to the language list). Its examples want a brand's tie to the company "clearly mentioned on external websites" (step 10). |

What round 3 changed against round 2: step 13 subscribes the new Messaging
account before the cutover (the read r2's MINOR 1); step 10 says the DBA is
the founders' plan and quotes the one Meta rule on display names (the
bead's draft clause, "the legal name or a registered DBA", was not Meta's
wording, so the page quotes Meta instead); step 7 quotes the Graph
reference's own words (the read r2's NOTE 1, the round-2 row above); step 3
names both accounts (NOTE 3).

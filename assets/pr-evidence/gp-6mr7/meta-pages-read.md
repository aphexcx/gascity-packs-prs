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

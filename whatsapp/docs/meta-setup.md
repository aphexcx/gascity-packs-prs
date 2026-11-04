# Meta setup for the Dynasty number (Afik's hand)

This page opens the Dynasty WhatsApp business account once, and right. Each
step names the Meta dashboard path and the Meta page it was read on,
2026-11-04. Meta moves its dashboard. Where the screen differs, the cited
page is the source of record.

On 2026-11-04 Taylor, at the keyboard, and the mayor, by Meta's API, did
most of week one. Each step is marked:

- **DONE**: done on 11/4. Confirm what the step names and redo nothing. A
  second app, system user or token would not be the one the gateway holds.
  The way to make it again stays under the step, for the day it is missing.
- **WAITING**: waits on what the step names. Nothing to do until it lands.
- **YOURS**: your hand, now or when its turn comes.

The ids on this page are Meta's names for things, not secrets. The token,
the app secret and the verify token are never on this page or in a chat:
the mayor holds them in files and sets them in the gateway. The gateway is
live for the test week at `https://whatsapp-gateway.houmanoids.workers.dev`
(deployed 11/4); `whatsapp.dynasty.computer` follows the move of the
dynasty.computer zone (step 7).

| Step | State on 11/4 | Next |
|---|---|---|
| 1. Business portfolio | DONE | nothing |
| 2. The app | DONE | nothing |
| 3. The accounts | DONE for week one | nothing |
| 4. The test number | DONE | nothing |
| 5. The permanent token | DONE | nothing |
| 6. The hand-off | DONE | nothing |
| 7. The webhook | DONE by the API; the custom domain WAITING | the mayor, at the zone move |
| 8. Live mode | YOURS | read the two pages, then **Publish** |
| 9. The template | filed; WAITING on Meta's review; classed Marketing | then YOURS, with Taylor |
| 10. Business verification and the DBA | submitted; WAITING | Meta; the accountant |
| 11 to 15. The real line | WAITING on step 10 and the first week | yours and the mayor's, in order |

## Week one, on Meta's test number

1. **Business portfolio.** DONE. The app, the WhatsApp Business account and
   the test number live in the one business portfolio, **Houdini**
   (1997564454198611), which belongs to Houmanoids Inc. Meta would not let
   Taylor add a second portfolio on 11/4, and none is needed: an owner sees
   the number's display name and business profile, never the portfolio's
   name. Check: [Business settings](https://business.facebook.com/latest/settings)
   lists you as an admin.
   Source: [Get started, step 1](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
2. **The app.** DONE. **Dynasty Computer**, app id 1705864634866180, use
   case **Connect with customers through WhatsApp**, in the Houdini
   portfolio. Check: the [App Dashboard](https://developers.facebook.com/apps)
   lists it, and it opens on **Customize use case > Connect on WhatsApp**.
   To make it again: **Create App** > app name and email > use case
   **Connect with customers through WhatsApp** > **Next** > the portfolio >
   the publishing requirements > **Next** > confirm > **Create app**.
   Source: [Get started, step 1](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
3. **The accounts.** DONE for week one. The WhatsApp Business account
   1706944460848228, named "Test Messaging Account" in Meta's panel, holds
   the test number, the template (step 9) and the webhook subscription
   (step 7). Meta now splits an account in two: the WhatsApp account holds
   the number and the display name, the Messaging account holds the
   templates and the billing. The real line brings its own pair (step 11).
   Check: **API Setup** shows the account's id with the test number. To
   make it again: Quickstart > **Start using the API** > **API Setup** >
   connect a Messaging account, or **Create a WhatsApp Business account**.
   Sources: [Get started, step 2](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started);
   [About the platform, WhatsApp accounts and Messaging accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform).
4. **The test number.** DONE. +1 555 164-0489, phone number id
   1429503880250022 (the id is not the phone number). Taylor's phone is its
   allowed recipient, and a test message from it reached that phone on 11/4
   (Taylor, 21:35Z). A test number has relaxed limits and needs no payment
   method to send templates. Check: **API Setup** > **From** shows it. To
   add another test phone: **API Setup** > **To**. The design read the
   recipient cap as five phones on 11/3; the pages read 11/4 state no
   number. Sources: [Get started, step 3](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started);
   [About the platform, Test resources](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform).
5. **The permanent token.** DONE. The system user **dynastywhatsapp**
   (61596426292660) has the **Admin** role, with the app assigned to it.
   Its token is a system user token that never expires, with the WhatsApp
   permissions, and the gateway sends with it. Check: [Business settings](https://business.facebook.com/latest/settings) >
   **System users** shows dynastywhatsapp with the app among its assets.
   Generate no second token: the gateway needs only this one. An Admin
   system user has full access to every WhatsApp account and Messaging
   account in the portfolio, the real line's included; an Employee one
   needs **both** accounts assigned by hand ("The panel does not
   cascade."). A missing grant shows later as Meta's error code `200` on a
   send. To make it again: **System users** > **Add+** > the **Admin** role >
   **Assign assets** > the app with **Manage app**, until it shows **Full
   control** (before that, **Generate token** says "No permissions
   available") > **Generate token** > the app > an expiration preference
   that never expires > `business_management`,
   `whatsapp_business_management` and `whatsapp_business_messaging`. Sources: [Access tokens, System user access tokens](https://developers.facebook.com/documentation/business-messaging/whatsapp/access-tokens);
   [Get started, step 5](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
6. **The hand-off.** DONE. The mayor holds the token, the app secret (on
   the app's **Basic settings** page) and the phone number id, and set the
   gateway's five secrets on 11/4; the other two, the verify token and the
   gateway's admin token, it made itself. Nothing is left to hand over. The
   rule stays for any later value: a sealed channel (1Password, or in
   person), never Slack, and nothing from Meta ever goes onto a city's box.
   Sources: [Basic settings](https://developers.facebook.com/documentation/development/create-an-app/app-dashboard/basic-settings);
   [Get started, step 3](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
7. **The webhook.** DONE by the API on 11/4; nothing to paste. The mayor
   subscribed the WhatsApp Business account to the app
   (`POST /<WABA_ID>/subscribed_apps`), then set the app's webhook with the
   app token (`POST /<APP_ID>/subscriptions`: object
   `whatsapp_business_account`, field `messages`, the callback
   `https://whatsapp-gateway.houmanoids.workers.dev/meta/webhook` and the
   verify token). Meta checked the callback on that call, and the
   subscription reads active. Check: the app's **Configuration** panel
   ([App Dashboard](https://developers.facebook.com/apps) > **Use cases** >
   **Customize** > **Configuration**; for other apps **WhatsApp** >
   **Configuration**) shows that **Callback URL** with **messages**
   subscribed. Save nothing there: a save runs Meta's check again with
   whatever is in the fields.

   **The custom domain.** WAITING on the zone move. `whatsapp.dynasty.computer`
   answers once the dynasty.computer zone is in the Houmanoids Cloudflare
   account. Then the mayor deploys the gateway on it, checks
   `https://whatsapp.dynasty.computer/healthz`, and points the callback at
   `https://whatsapp.dynasty.computer/meta/webhook` by the same API call,
   with the same verify token. You confirm the new URL in
   **Configuration**. Meta's Graph API reference for that call says
   WhatsApp webhooks must be set in the App Dashboard; Meta's WhatsApp page
   names the call as the alternative, and the 11/4 call took. If it is ever
   refused, set the URL in **Configuration**, with the verify token the
   mayor sends by a sealed channel. Sources: [Create a webhook endpoint, Configure webhooks](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/create-webhook-endpoint);
   [Manage webhooks](https://developers.facebook.com/documentation/business-messaging/whatsapp/solution-providers/manage-webhooks);
   [Application subscriptions](https://developers.facebook.com/docs/graph-api/reference/application/subscriptions/);
   [Webhooks overview](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/overview).
8. **Live mode.** YOURS. "Make sure your app is in **Live** mode; some
   webhooks will not be sent if your app is in **Dev** mode." App Dashboard
   > **Publish** in the left menu > review > **Publish**. Publishing asks for
   an app icon, a privacy policy URL and a data deletion URL. Both URLs
   answer as of 11/4 22:47Z, from dynasty PR 20 (merged 22:45Z):
   `https://dynasty.computer/privacy` and
   `https://dynasty.computer/data-deletion`. PR 20 wrote them as drafts for
   the founders, so read both before you publish. The icon is Dynasty's
   mark: `app/icon.svg` in the dynasty repo exports at any size (its PNGs
   are 48 and 180 pixels, too small for an app icon). Sources:
   [Webhooks overview, Troubleshooting](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/overview);
   [WhatsApp use case, Publish](https://developers.facebook.com/documentation/development/create-an-app/whatsapp-use-case).
9. **The one template, `dynasty_update`.** Filed on 11/4; WAITING on Meta's
   review; then YOURS, with Taylor. The mayor filed it by the API at 21:17Z
   as category **Utility**, in `en_US` (1539785464624038) and `pt_BR`
   (1442968537981131), with the bodies and samples below. Both read
   **PENDING**, and Meta classed both **MARKETING** at the filing (the
   API's read, 21:5xZ). So the Marketing case below is the state in effect,
   not a later risk. Review takes up to 24 hours. Sources:
   [Templates overview](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/overview);
   [Template review](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/template-review);
   [Template categorization](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/template-categorization);
   [Template management](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/template-management).

   While it is pending there is nothing to do: "Only templates with an
   APPROVED, REJECTED, or PAUSED status can be edited." Once Meta decides,
   the one action in front of you and Taylor is a category review, after a
   change of wording if you want one:

   - **The review.** [WhatsApp Manager > Message templates](https://business.facebook.com/latest/whatsapp_manager/message_templates) >
     the banner on the template > **Go to Business Support** > **Template
     Category Updates** > select both languages > **Request Review**, "up
     to 60 days from the date the category was updated".
   - **The wording.** Utility means "non-promotional" and "specific to or
     requested by the user (clearly related to their order, account,
     services, or transactions)". An approved template can be edited "up to
     10 times in a 30-day window, or 1 time in a 24-hour window", and an
     edit goes through Meta's category check and review again. The adapter
     sends only the template's name, its language and the one parameter, so
     new wording changes no code as long as it keeps one parameter; the
     mayor's skill and `gc whatsapp publish`'s help quote the first sentence
     and change with it.

   Never delete it to file it again: "If you delete an approved template,
   you cannot create a new template with the same name for 30 days", and
   the adapter sends this name. If both languages come back status
   **APPROVED** with category **UTILITY** after all, nothing more is needed.

   Meta rejects a template that starts or ends with a variable: "The
   message template cannot start or end with a parameter (dangling
   parameters are not allowed)." The design's body, `Your city has an
   update: {{1}}`, ends with one, so each body gets a fixed closing line.

   `en_US` body, then its sample for `{{1}}`:

   ```
   Your city has an update: {{1}}

   Reply here to continue.
   ```

   ```
   The shipment cleared customs this morning.
   ```

   `pt_BR` body, then its sample for `{{1}}`:

   ```
   Sua cidade tem uma novidade: {{1}}

   Responda aqui para continuar.
   ```

   ```
   O pedido foi liberado na alfândega hoje de manhã.
   ```

   A city sends the language its env names (`WHATSAPP_UPDATE_LANGUAGE`,
   `en_US` by default; a Brazilian city sets `pt_BR`).

   **Why Marketing is a blocker, not a price change.** "WhatsApp does not
   currently deliver marketing template messages to WhatsApp users with
   United States phone numbers". Taylor's test phone and the first two
   customers are US numbers, so a marketing `dynasty_update` never reaches
   them. Elsewhere it is capped per user (error `131049`) and refused to
   anyone who turned marketing messages off (error `131050`). Meta also
   re-checks categories over time, so look at the category again before
   each step that depends on it. Sources: [Template categorization](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/template-categorization);
   [Per-user marketing template limits, United States phone numbers](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/marketing-templates/per-user-limits);
   [Error codes](https://developers.facebook.com/documentation/business-messaging/whatsapp/support/error-codes).

## After the first week, the real line

The real line waits on step 10, which is under way now.

10. **Business verification and the DBA.** WAITING. Taylor submitted Meta's
    business verification on 11/4 at 21:30Z; the API read it as pending at
    21:5xZ. Taylor is asking the founders' accountant to file a DBA for
    "Dynasty Computer", the name the app carries, under Houmanoids Inc. Keep
    the filing at hand in case Meta's review asks how that name and the
    legal business connect. The display name and the real line wait on
    both. Verification matters three ways. "Newly created business
    portfolios have a messaging limit of 250", and verifying is one path to
    2,000. It lifts the cap of two registered numbers to 20. And a number's
    display name is checked "When you reach a higher messaging limit".
    YOURS only if Meta asks for documents. Check: App Dashboard >
    **Settings** > **Basic** > **Verification** shows whether the app's
    business is verified. Sources:
    [Messaging limits](https://developers.facebook.com/documentation/business-messaging/whatsapp/messaging-limits);
    [Business phone numbers](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-phone-numbers/phone-numbers);
    [Display names](https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names);
    [Business verification](https://developers.facebook.com/docs/development/release/business-verification).
11. **The line.** YOURS, after step 10. A phone line on no WhatsApp app
    (personal or Business) that can receive an SMS or a voice call:
    "Numbers already in use with WhatsApp cannot be registered unless they
    are deleted first." Add it on the app's **API Setup** page (the left
    menu under **Customize use case > Connect on WhatsApp**; Meta's page
    writes the path as **WhatsApp** > **API Setup**), with the display name
    **Dynasty**, and verify it by the SMS code. Adding a production number
    there creates a new WhatsApp account for it and a new Messaging account
    for its templates and billing. Sources: [Business phone numbers](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-phone-numbers/phone-numbers);
    [WhatsApp Business Accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/whatsapp-business-accounts).
12. **Register it.** WAITING on step 11; the mayor makes the call. "You can
    only register a number via the API": not in WhatsApp Manager, not in
    the App Dashboard. It is one call,
    `POST https://graph.facebook.com/v26.0/<PHONE_NUMBER_ID>/register` with
    `{"messaging_product":"whatsapp","pin":"<6 digits>"}`, the pin being the
    number's 6-digit two-step verification PIN. Choose the PIN and hand it
    to the mayor by a sealed channel; the mayor holds the token, so it
    makes the call.
    Source: [Register a phone number](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-phone-numbers/registration).
13. **The template again, on the new Messaging account.** WAITING on step
    11; the mayor can file it by the API, as on 11/4. Templates live on the
    Messaging account, and the real line came with a new one (step 11).
    File `dynasty_update` there with the same name, bodies and samples (by
    hand: [WhatsApp Manager > Message templates](https://business.facebook.com/latest/whatsapp_manager/message_templates) >
    that Messaging account > create, category **Utility**, languages
    `en_US` and `pt_BR`), and wait until both languages read status
    **APPROVED** with category **UTILITY**: a template can show Utility
    while still pending, and a Marketing approval stops here (step 9). Only
    then does the mayor point
    the gateway at the new number's phone number id (one gateway secret
    changes; no city re-keys). Sources: [About the platform, WhatsApp accounts and Messaging accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform);
    [WhatsApp Business Accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/whatsapp-business-accounts).
14. **The name and the photo.** YOURS. [WhatsApp Manager](https://business.facebook.com/latest/whatsapp_manager/) >
    **Account tools** > **Phone numbers** > the number > **Profile**:
    display name **Dynasty** and a plain photo. Meta checks the display name
    only when the number reaches a higher messaging limit, and only an
    approved name shows at the top of a chat; until then the owner sees the
    name they saved, "Dynasty". After an approved name change the number
    must be registered again (step 12) within 14 days. Sources: [Display names](https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names);
    [Business profiles](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-profiles).
15. **A payment method.** YOURS, a check. Taylor added a payment method on
    11/4 (21:35Z). The real line's Messaging account is a new one (step
    11): confirm in [Billing Hub](https://business.facebook.com/latest/billing_hub/)
    that it has a payment method before the line takes traffic. Meta charges
    for templates, and since 2026-10-01 "if businesses do not have a payment
    method for their Messaging account(s), Meta will deliver service
    messages within the free tier but not deliver them after the free tier
    has been used" (1,000 a month per number). Source: [Pricing, Free tier of monthly service messages](https://developers.facebook.com/documentation/business-messaging/whatsapp/pricing).

## Two rules that stay with the account

**What the account is.** Wherever Meta asks what the number is for (the
business profile, app review), describe it as a business operations
assistant for registered owners, never a public chatbot. Meta's terms of
2026-01-15 allow general-purpose AI assistants from "AI Providers" only where
the law requires it. Source: [AI Providers](https://developers.facebook.com/documentation/business-messaging/whatsapp/pricing/ai-providers).

**Opt-in.** An owner's consent to proactive messages (the window-keeper's
daily update and reminders) is recorded by the city at the invite
(`gc whatsapp invite --proactive-consent`), never assumed. `dynasty_update`
is the only message the mayor sends outside an open window, and only for an
actual update. Source: [Obtaining user opt-in](https://developers.facebook.com/documentation/business-messaging/whatsapp/getting-opt-in).

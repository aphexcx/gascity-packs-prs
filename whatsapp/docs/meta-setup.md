# Meta setup for the Dynasty number (Afik's hand)

This page opens the Dynasty WhatsApp business account once, and right. Each
step names the Meta dashboard path and the Meta page it was read on,
2026-11-04. Meta moves its dashboard. Where the screen differs, the cited
page is the source of record.

You do the steps in Meta. The mayor deploys the gateway at
whatsapp.dynasty.computer and sets its secrets, following the gateway's
README (section "Deploy"), and can make the one API call in step 11 once it
holds the token. Step 7 waits for the gateway, because Meta checks the
webhook URL the moment you save it.

## Week one, on Meta's test number

1. **Business portfolio.** Use Houmanoids' business portfolio:
   [Business settings](https://business.facebook.com/latest/settings).
   Source: [Get started, step 1](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
2. **The app.** [App Dashboard](https://developers.facebook.com/apps) >
   **Create App** > app name and email > use case **Connect with customers
   through WhatsApp** > **Next** > the Houmanoids portfolio > the publishing
   requirements (likely none yet) > **Next** > confirm > **Create app**. You
   land on **Customize use case > Connect on WhatsApp > Quickstart**.
   Source: [Get started, step 1](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
3. **The accounts.** Quickstart > **Start using the API** > **API Setup** >
   connect a Messaging account, or **Create a WhatsApp Business account**,
   named for Dynasty. Meta now splits this in two: the WhatsApp account
   holds the number and the display name, the Messaging account holds the
   templates and the billing. Sources: [Get started, step 2](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started);
   [About the platform, WhatsApp accounts and Messaging accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform).
4. **The test number.** Meta creates it with the app, with a test WhatsApp
   account and a test Messaging account. **API Setup** > **From**: the test
   number; **To**: add Taylor's phone, then any other test phone. A test
   number has relaxed limits and needs no payment method to send templates.
   The design read the recipient cap as five phones on 11/3; the pages read
   today state no number. Sources: [Get started, step 3](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started);
   [About the platform, Test resources](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform).
5. **The permanent token.** [Business settings](https://business.facebook.com/latest/settings) >
   **System users** > **Add+** > create the system user with the **Admin**
   role, which by default has full access to every WhatsApp account and
   Messaging account in the portfolio, the real line's included. (An
   Employee system user needs **both** accounts assigned by hand: "The
   panel does not cascade.") Select it > **Assign assets** > the app with
   **Manage app**, wait until it shows **Full control** > **Generate token**
   > select the app > choose an expiration preference that never expires
   (the gateway holds this one token) > permissions `business_management`,
   `whatsapp_business_management` and `whatsapp_business_messaging`. Copy it
   once and keep it for the hand-off. A missing grant shows later as Meta's
   error code `200` on a send. Sources: [Access tokens, System user access tokens](https://developers.facebook.com/documentation/business-messaging/whatsapp/access-tokens);
   [Get started, step 5](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
6. **The app secret and the phone number id, then the hand-off.** The app
   secret is on the app's **Basic settings** page in the
   [App Dashboard](https://developers.facebook.com/apps). The phone number
   id is shown on **API Setup** with the test number ("retain both your
   test phone number ID and Messaging account ID"); it is not the phone
   number. Hand the mayor these two and the token by a sealed channel
   (1Password, or in person), never Slack. The mayor sets them in the
   gateway, which takes five secrets in all (its README, section "Deploy",
   names them); the other two, the verify token and the gateway's admin
   token, the mayor generates. Nothing from Meta ever goes onto a city's
   box. Sources: [Basic settings](https://developers.facebook.com/documentation/development/create-an-app/app-dashboard/basic-settings);
   [Get started, step 3](https://developers.facebook.com/documentation/business-messaging/whatsapp/get-started).
7. **The webhook, once the mayor says the gateway is live.** The mayor
   sends you the verify token by the same sealed channel. For an app made
   with the **Connect with customers through WhatsApp** use case the panel
   is [App Dashboard](https://developers.facebook.com/apps) > **Use cases** >
   **Customize** > **Configuration** (for other apps, **WhatsApp** >
   **Configuration**). **Callback URL**:
   `https://whatsapp.dynasty.computer/meta/webhook`. **Verify token**: the
   mayor's string. Save, then subscribe to the **messages** field. Sources:
   [Create a webhook endpoint, Configure webhooks](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/create-webhook-endpoint);
   [Webhooks overview](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/overview).
8. **Live mode.** "Make sure your app is in **Live** mode; some webhooks
   will not be sent if your app is in **Dev** mode." App Dashboard >
   **Publish** in the left menu > review > **Publish**. Publishing asks for
   an app icon, a privacy policy URL and a data deletion URL. Ask the mayor
   for Dynasty's two URLs before this step; if Dynasty has no such pages
   yet, that is the open item that blocks it. Sources:
   [Webhooks overview, Troubleshooting](https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/overview);
   [WhatsApp use case, Publish](https://developers.facebook.com/documentation/development/create-an-app/whatsapp-use-case).
9. **The one template, `dynasty_update`.** [WhatsApp Manager > Message templates](https://business.facebook.com/latest/whatsapp_manager/message_templates) >
   (the Messaging account the number sends from) > create: category
   **Utility**, name `dynasty_update`, languages `en_US` and `pt_BR`, with
   the bodies and samples below. Approval takes up to 24 hours. Sources:
   [Templates overview](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/overview);
   [Template review](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/template-review);
   [Template categorization](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/template-categorization).

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

   The adapter sends only the template's name, its language and the one
   parameter, so the closing line changes no code. A city sends the language
   its env names (`WHATSAPP_UPDATE_LANGUAGE`, `en_US` by default; a
   Brazilian city sets `pt_BR`).

   **The template must be approved as Utility. A Marketing approval is a
   blocker, not a price change.** Meta can file a Utility submission as
   marketing ("the template is approved as `MARKETING`"), and "WhatsApp does
   not currently deliver marketing template messages to WhatsApp users with
   United States phone numbers". Taylor's test phone and the first two
   customers are US numbers, so a marketing `dynasty_update` never reaches
   them; elsewhere it is capped per user and refused to anyone who turned
   marketing messages off (error `131050`). If it comes back as Marketing,
   change the wording and ask for a review (open for 60 days after the
   category was set) before anyone relies on it. Meta also re-checks
   categories over time, so look at the category again before each step
   that depends on it. Sources: [Template categorization](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/template-categorization);
   [Per-user marketing template limits, United States phone numbers](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/marketing-templates/per-user-limits);
   [Error codes](https://developers.facebook.com/documentation/business-messaging/whatsapp/support/error-codes).

## After the first week, the real line

10. **The line.** A phone line on no WhatsApp app (personal or Business)
    that can receive an SMS or a voice call: "Numbers already in use with
    WhatsApp cannot be registered unless they are deleted first." Add it on
    the app's **API Setup** page (the left menu under **Customize use case >
    Connect on WhatsApp**; Meta's page writes the path as **WhatsApp** >
    **API Setup**), with the display name **Dynasty**, and verify it by the
    SMS code. Adding a production number there creates a new WhatsApp
    account for it and a new Messaging account for its templates and
    billing. Sources: [Business phone numbers](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-phone-numbers/phone-numbers);
    [WhatsApp Business Accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/whatsapp-business-accounts).
11. **Register it.** "You can only register a number via the API": not in
    WhatsApp Manager, not in the App Dashboard. It is one call,
    `POST https://graph.facebook.com/v26.0/<PHONE_NUMBER_ID>/register` with
    `{"messaging_product":"whatsapp","pin":"<6 digits>"}`, the pin being the
    number's 6-digit two-step verification PIN. Choose the PIN and keep it
    with the token. The mayor can make the call, since it holds the token.
    Source: [Register a phone number](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-phone-numbers/registration).
12. **The template again, on the new Messaging account.** Templates live on
    the Messaging account, and the real line came with a new one (step 10).
    Repeat step 9 there, with the same name, bodies and samples, and wait
    for the approval **as Utility** (a Marketing approval stops here, step
    9). Only then does the mayor point the gateway at the new
    number's phone number id (one gateway secret changes; no city
    re-keys). Sources: [About the platform, WhatsApp accounts and Messaging accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform);
    [WhatsApp Business Accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/whatsapp-business-accounts).
13. **The name and the photo.** [WhatsApp Manager](https://business.facebook.com/latest/whatsapp_manager/) >
    **Account tools** > **Phone numbers** > the number > **Profile**:
    display name **Dynasty** and a plain photo. Meta checks the display name
    only when the number reaches a higher messaging limit, and only an
    approved name shows at the top of a chat; until then the owner sees the
    name they saved, "Dynasty". After an approved name change the number
    must be registered again (step 11) within 14 days. Sources: [Display names](https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names);
    [Business profiles](https://developers.facebook.com/documentation/business-messaging/whatsapp/business-profiles).
14. **A payment method.** Add one to the real line's Messaging account in
    [Billing Hub](https://business.facebook.com/latest/billing_hub/) before
    the line takes traffic. Meta charges for templates, and since 2026-10-01
    "if businesses do not have a payment method for their Messaging
    account(s), Meta will deliver service messages within the free tier but
    not deliver them after the free tier has been used" (1,000 a month per
    number). Source: [Pricing, Free tier of monthly service messages](https://developers.facebook.com/documentation/business-messaging/whatsapp/pricing).

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

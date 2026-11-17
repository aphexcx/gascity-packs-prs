# Membership API references

Read 2026-11-16 from Google's public Chat REST reference. These are reference
pages, not calls to a Chat service or customer account.

- [Membership resource](https://developers.google.com/workspace/chat/api/reference/rest/v1/spaces.members):
  `createTime` is "Optional. Immutable. The creation time of the membership,
  such as when a member joined or was invited to join a space." It is output
  only except for historical imports. `state` is the membership's output state.
- [Get membership](https://developers.google.com/workspace/chat/api/reference/rest/v1/spaces.members/get):
  `GET https://chat.googleapis.com/v1/{name=spaces/*/members/*}`;
  resource format `spaces/{space}/members/{member}`. The response is a
  Membership. User authentication supports `chat.memberships.readonly`, which
  is already in both adapter scope lists. This change uses the canonical
  authenticated user ID as the member segment, not the app membership alias.

The references do not establish that the membership creation timestamp is
identical to the message that caused the join. The bounded first-read margin
does not depend on their being equal.

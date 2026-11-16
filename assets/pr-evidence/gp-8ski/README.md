# Room invitation acceptance — gp-8ski

UNPROVEN: all Google and key holder requests in this work use local fakes.
Google does not document that a self-create accepts an invitation. A proof
call must settle U1-U4 before this draft is considered for release.

## Answers modeled by the fake

| Read or create answer | Command result | Evidence |
| --- | --- | --- |
| Read: JOINED | already_member; no permission request or write | DOCUMENTED G1; resource shape G8 |
| Read: INVITED | proceed to permission/owner checks | ASSUMED U2; state name DOCUMENTED G1 |
| Read: NOT_A_MEMBER or missing membership (404 NOT_FOUND) | no_invitation | ASSUMED U3; state meaning DOCUMENTED G1 |
| Read: any other membership state | no_invitation | ASSUMED U2/U3; fail closed |
| Create self: JOINED | joined | ASSUMED U1; request shape DOCUMENTED G3/G4 |
| Create: 400 FAILED_PRECONDITION | refused_invited | ASSUMED U4; G10 documents the adder, not the invitee |
| Create: 403 PERMISSION_DENIED | refused_invited | ASSUMED U4; G10 documents the adder, not the invitee |
| Create: 404 NOT_FOUND | no_invitation | ASSUMED U3 |
| Create: still INVITED | refused_invited | ASSUMED U1 |
| Unrecognized HTTP/status pair | google_unknown; HTTP and status word, nothing changed | ASSUMED U1-U4; fail closed |
| Lost response/transport error | google_no_answer; retry begins with a read | ASSUMED U1-U4 |

Google references G1-G11 are the Membership resource, create, RPC
CreateMembershipRequest, patch, Chat authorization, delegated user
authentication, membership get, spaces list/search, create-members, and
Workspace domain-wide delegation setup documentation, respectively.
The key holder's ability to grant a new scope (U5) remains outside this repository.

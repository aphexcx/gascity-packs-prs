Usage: gc gchat join --space spaces/ID --request
       gc gchat join --space spaces/ID --yes spaces/DM/messages/ID
       gc gchat join --check

Use --help or -h to display this help.

Name exactly one room by its id from the invitation, never its display name.
--request checks the invitation and permission, then returns a one-time code
valid for 24 hours. A new request for this room replaces its previous code.
Ask the owner in a direct message, naming the room and inviter and explaining
that everyone in the room can write to the identity. With the owner's yes,
pass the name of that direct message to --yes; the adapter verifies its sender,
time, direct-message space, and code for this room. A used approval is dead.

--check only checks the permission; it touches no room and needs no approval.
Desktop sign-in is not supported. The write permission is requested only by
this command; updating the pack alone does not change standing permissions.

A retry reads membership first. An already joined room needs no approval or
write token. After a failed write, request a new code and approval if the room
is still invited. Do not ask again after a no or silence unless the owner
raises the room. The actual Google self-join behavior remains UNPROVEN;
the implementation has been exercised only against fakes.

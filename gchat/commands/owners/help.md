Usage: gc gchat owners
       gc gchat owners --remove users/ID --request
       gc gchat owners --remove users/ID --yes spaces/DM/messages/ID
       gc gchat owners --add

Use --help or -h to display this help.

With no flag it lists who is an owner. On a box, owners come from root's
owners file alone (root's directory /run/gchat-owner-claim is present): each
Chat user id, since when, how (page-code) and the claim id of the code that
made it. Any owner the settings name is ignored there. Where root's
directory is absent, as in a city that is not a box, it lists the settings'
entries (GCHAT_OWNERS) and says that they come from the settings; no command
adds or removes one of those, and no owner code is ever taken there.

A new owner comes by a one-time code from the owner page, sent alone to the
mayor in a direct chat, and by nothing else. --add, with any other flag,
asks nothing, changes nothing and answers add_by_page.

To remove an owner, --remove users/ID --request returns a one-time code for
that one change, valid for 24 hours; a new request replaces the old one. Ask
an owner in that owner's own direct chat, naming the person and the change.
The owner replies with the code alone. Then pass the name of that reply to
--remove users/ID --yes. The adapter checks the yes as the join does: the
sender is an owner and not the mayor's own account, the reply is in a
direct chat, written after the request and before it expired, and carries
the code alone. Then it asks root's door to remove the person, once. An
owner may remove themself, and the last owner may be removed.

The answer removed means root removed the person. door_unavailable means
the box did not confirm the removal: run gc gchat owners to see who is an
owner now, and if the person is still listed, make a new request and get a
new yes. A used yes is dead either way.

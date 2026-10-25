CLEAN — retained tombstones prevent late handlers from restoring invalidated
excerpts. Cache updates share the mutex and 4,096-entry cap; invalidated entries
return no parent text.

The new test covers cold/warm invalidation, late writes, and capacity. No
additional actionable issue found. No edits or tests run.

CLEAN — the pointer's bytes are reserved before body trimming. Positive
budgets remain enabled when the reservation exceeds the limit; zero and
negative budgets remain disabled. Protected content retains the existing
overflow behavior.

The regression test covers the relevant boundaries. No actionable issues
found; no edits or tests run.

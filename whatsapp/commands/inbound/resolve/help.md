# gc whatsapp inbound resolve

```
gc whatsapp inbound resolve <rid> --delivered
gc whatsapp inbound resolve <rid> --dead
```

The operator's one path out of a TERMINAL HOLD. gc answers a message whose
pasted draft lost its ownership with a pending receipt marked terminal; gc's
contract is stop polling, do not redeliver, operator attention required. The
adapter holds such a message apart: never posted again, never read back, the
gateway keeps the body, the doctor row `held` lists it. Read the envelope
(`<city>/.gc/whatsapp/inbound_journal.jsonl`, the `received` line of that rid),
look at the mayor's session, then decide:

- `--delivered`: the mayor did take it. The adapter acks the gateway.
- `--dead`: it was not taken. A dead letter under reason `held_terminal` with
  the envelope, then the ack.

The verb prints the phone's last four digits, the receipt id and the time,
never the text, before it acts. The ack is owed until the gateway confirms it
(`gc whatsapp status` shows acks owed).

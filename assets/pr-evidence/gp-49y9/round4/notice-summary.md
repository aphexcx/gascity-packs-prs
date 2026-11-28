| Row | Baseline | Member final record | Control final record | Ambient Removed records |
| --- | --- | --- | --- | --- |
| refused | red | ['unreachable', 0, 'unreachable', 0, 0, 0, 1, 1] | ['unreachable', 0, 'unreachable', 0, 0, 0, 1, 1] | none |
| gate_and_read_422 | red | ['-', 0, '-', 1, 0, 0, 1, 1] | ['-', 0, '-', 1, 0, 0, 1, 1] | none |
| answer_lost | red | ['-', 0, '-', 1, 1, 0, 1, 1] | ['-', 0, '-', 1, 1, 0, 1, 1] | none |
| failed | red | ['failed', 0, 'failed', 0, 0, 0, 1, 1] | ['failed', 0, 'failed', 0, 0, 0, 1, 1] | none |
| held | red | ['failed', 0, 'failed', 0, 0, 0, 1, 1] | ['failed', 0, 'failed', 0, 0, 0, 1, 1] | none |
| fourth_partial | red | ['partial', 0, '-', 1, 1, 0, 1, 1] | ['partial', 0, '-', 1, 1, 0, 1, 1] | none |
| no_route | red | ['no_route', 0, '-', 1, 0, 0, 1, 1] | ['no_route', 0, '-', 1, 0, 0, 1, 1] | none |
| delivered_ok | red | ['delivered', 1, '-', 0, 0, 0, 0, 0] | ['delivered', 1, '-', 0, 0, 0, 0, 0] | none |
| delivered_down | red | ['delivered', 1, '-', 0, 0, 0, 1, 1] | ['delivered', 1, '-', 0, 0, 0, 1, 1] | none |
| pending | red | ['pending', 0, '-', 0, 0, 0, 0, 0] | ['pending', 0, '-', 0, 0, 0, 0, 0] | none |
| whole_paste_terminal | red | ['delivered', 1, '-', 0, 0, 0, 0, 0] | ['delivered', 1, '-', 0, 0, 0, 0, 0] | none |
| landed_whole | red | ['delivered', 1, '-', 0, 0, 0, 0, 0] | ['delivered', 1, '-', 0, 0, 0, 0, 0] | none |
| terminal_not_whole | red | ['pending', 0, '-', 1, 1, 0, 1, 1] | ['pending', 0, '-', 1, 1, 0, 1, 1] | none |
| receipt_failed | red | ['failed', 0, 'failed', 0, 0, 0, 1, 1] | ['failed', 0, 'failed', 0, 0, 0, 1, 1] | none |
| restart_422 | red | ['-', 0, '-', 1, 0, 0, 1, 1] | ['-', 0, '-', 1, 0, 0, 1, 1] | none |
| restart_no_route | red | ['no_route', 0, '-', 1, 0, 0, 1, 1] | ['no_route', 0, '-', 1, 0, 0, 1, 1] | none |
| restart_delivered_ok | red | ['delivered', 1, '-', 0, 0, 0, 0, 0] | ['delivered', 1, '-', 0, 0, 0, 0, 0] | none |
| restart_delivered_down | red | ['delivered', 1, '-', 0, 0, 0, 1, 1] | ['delivered', 1, '-', 0, 0, 0, 1, 1] | none |
| restart_whole_paste | red | ['delivered', 1, '-', 0, 0, 0, 0, 0] | ['delivered', 1, '-', 0, 0, 0, 0, 0] | none |
| accept | red | ['-', 0, '-', 0, 0, 1, 0, 0] | ['-', 0, '-', 0, 0, 1, 0, 0] | spaces/room/messages/m003 |
| replay | red | ['-', 0, '-', 0, 0, 1, 0, 0] | ['-', 0, '-', 0, 0, 1, 0, 0] | spaces/room/messages/m003 |
| two_mentions | red | ['-', 0, '-', 1, 0, 0, 1, 1] | ['-', 0, '-', 1, 0, 0, 1, 1] | none |
| restart_before_urgent_freeze | red | ['-', 0, '-', 1, 0, 0, 1, 1] | ['-', 0, '-', 1, 0, 0, 1, 1] | none |
| act_cap_owns_mention_pin | pin | ['-', 0, '-', 1, 0, 0, 1, 1] | ['-', 0, '-', 1, 0, 0, 1, 1] | none |
| owner_change | red | ['unreachable', 0, 'unreachable', 1, 0, 0, 1, 1] | ['unreachable', 0, 'unreachable', 1, 0, 0, 1, 1] | spaces/room/messages/m001, spaces/room/messages/m003 |
| member_deleted | red | ['unreachable', 0, 'unreachable', 0, 0, 1, 0, 0] | ['unreachable', 0, 'unreachable', 0, 0, 1, 0, 0] | none |
| replay_then_owner_change | red | ['-', 0, '-', 0, 0, 1, 0, 0] | ['-', 0, '-', 0, 0, 1, 0, 0] | spaces/room/messages/m001, spaces/room/messages/m003 |
| window_off_pin | pin | ['-', 0, '-', 1, 0, 0, 1, 1] | ['-', 0, '-', 1, 0, 0, 1, 1] | none |

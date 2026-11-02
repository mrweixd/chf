# ABMF receipt storage

The charging account document stores its balance and one current receipt in
`chfLatest`. The same MongoDB update changes both. Before replacing the
receipt with a different session's result, ABMF copies the previous one to
`policyData.ues.chargingData.receipts`. The archive keeps the highest accepted
request number and its result for each account document ID and session hash.
A late writer cannot move that number backwards. If archiving fails, the
account update does not proceed.

A replay of the latest matching request returns its original result; older
request numbers and conflicting payloads are rejected. Archive storage grows
with distinct sessions. Do not apply a TTL or clear this collection while
those sessions can still be replayed. Back up and restore the account and
receipt collections together at a consistent point in time.

When upgrading from the branch that stored a map in `chfReceipts`, stop the
old ABMF writers before switching versions. On access, the new ABMF copies
legacy entries in batches of 128 and only then removes them from the account.
An interrupted migration can resume. Grant the ABMF database user read/write
access to both collections. This layout uses MongoDB update pipelines and
does not require multi-document transactions.

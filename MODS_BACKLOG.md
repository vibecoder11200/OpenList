# Mods backlog

Accepted but deliberately deferred. Do not pick these up without the owner's
go-ahead.

- [ ] `/mf/fast` alias + `mff` S3 bucket — small alias over the ~20 healthiest
  MediaFire accounts with the folder tree pre-created on every member. Cuts the
  384-way fan-out per operation, lets `put: quota` actually spread writes, and
  keeps MediaFire rate limits away from the main farm. The owner will request
  this when needed.
- [ ] Split the farm into ~100-account aliases (`auto`, `auto1`, `auto2`, …)
  for easier management — owner's plan, to be scheduled later ("chuyện này
  tính sau").

# Vendored code

- `crypto/`: golang.org/x/crypto v0.41.0, only the packages needed by `ssh` (BSD licence, see LICENSE).
- `sys/`: golang.org/x/sys v0.39.0, only `cpu`.

## Local change (ptscry)

`crypto/ssh/keys.go`: new package variable `SKRequiredFlags`. The Verify methods of
`sk-ecdsa-sha2-nistp256@openssh.com` and `sk-ssh-ed25519@openssh.com` refuse a signature whose
FIDO flags do not include all the bits in it. ptscry sets it to 0x05 (user present + user
verified, i.e. PIN/biometric: OpenSSH `verify-required`). Everything else is unmodified.

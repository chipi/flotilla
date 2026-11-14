# Host keys and certificates

**Status:** written 2026-11-13, before the first write at propose (ADR-0006 §11). None of these
procedures has been run on a real host yet. The e2e (`make test-e2e`) runs the pieces they are made
of (the CA and signing-key commands, `secrets.sh`, install, enrolment, revocation, remove) on a
stand-in host, but not a rotation from start to end.

The keys (custody in [Deployment](../architecture/deployment.md#keys-and-custody)):

| Key | Lives in | Expires | Used for |
| --- | --- | --- | --- |
| Root CA (`root.key`) | the operator's machine, offline | 10 years | signs intermediates |
| Intermediate CA (`intermediate.key`) | flotillad's secrets volume | 3 years | issues host certificates and flotillad's server certificate |
| Execution-signing key (`signing.key`, Ed25519) | flotillad's secrets volume | never; rotated | signs every execution |
| A host's key (`key.pem`) and certificate | the host, `/var/db/flotilla-host` | 30 days, renewed | the host's identity to flotillad |

What each side trusts:

- **A host agent** pins the root (`/etc/flotilla-host/root.crt`) and flotillad's name. It accepts any
  server certificate that chains to the root under that name. It accepts an execution only if it is
  signed by a key its config lists under `signing_keys` (one key, or two during a rotation), by
  key id.
- **flotillad** accepts a host's certificate only if it chains to the root and is exactly the
  certificate flotillad issued and recorded last for that host (its SHA-256 in the `hosts` table).
  Revoking a host clears that record.

The files on the operator's machine, in `$FLOTILLA_CONFIG/host-ca/`: `intermediate.crt`,
`intermediate.key`, `root.crt` and `signing.key`. `secrets.sh` (run by every deploy) copies them
into flotillad's secrets volume, and refuses to run if `root.key` is among them. A deploy recreates
flotillad when any of `intermediate.crt`, `root.crt` and `signing.key` changes, because flotillad
reads them only at start. The `flotilla ca` commands never overwrite a file, so each new key is
made in a new folder and then moved into place.

## Rotate the execution-signing key

When: on a schedule the operator chooses, or at once on any suspicion (then see
[If a key leaked](#if-a-key-leaked)). There is a window of two keys, so nothing stops.

1. **Make the new key**, offline, with an id the hosts do not use yet:

   ```bash
   flotilla ca signing-key ~/flotilla-keys/exec2 exec2
   ```

   It writes `exec2/signing.key` (mode 600) and prints the line for the hosts' configs.

2. **Add it to every host**, beside the old key. In each host's config (`signing_keys`, two keys at
   most):

   ```json
   "signing_keys": {"exec1": "<old>", "exec2": "<new>"}
   ```

   Then, per host, the reviewed config goes on with the version the host already runs:

   ```bash
   deploy/host/install.sh upgrade HOST --ref <its version> --os <os> --arch <arch> --config <its config>
   ```

   Check: `UPGRADE PASS`, and `flotilla host list` shows the host connected again. A host still
   without the new key refuses every execution signed with it (the case escalates).

3. **Switch flotillad.** Move the old `signing.key` out of `$FLOTILLA_CONFIG/host-ca/`, put the new
   one in, set `hosts.signing_key_id: exec2` in `flotillad.yaml`, and deploy. flotillad is recreated
   and signs with `exec2` from then on.

4. **Wait 10 minutes.** An envelope signed with `exec1` before the switch is valid for 10 minutes,
   and a host only runs it while it still lists `exec1`. Check that no case is in `executing`
   (`flotilla case list`).

5. **Remove the old key** from every host's config and upgrade each host again, as in step 2.

6. **Destroy the old key** on the operator's machine.

## Rotate the intermediate CA

When: before it expires (3 years; `openssl x509 -enddate -noout -in intermediate.crt`), or at once
on suspicion (then see [If a key leaked](#if-a-key-leaked)). The hosts do nothing.

1. **Make the new intermediate** under the same root, offline, into a new folder:

   ```bash
   flotilla ca intermediate ~/flotilla-keys/root ~/flotilla-keys/inter-2029
   ```

2. **Put it in place:** replace `intermediate.crt` and `intermediate.key` in
   `$FLOTILLA_CONFIG/host-ca/` with the new ones. `root.crt` and `signing.key` stay.

3. **Deploy.** flotillad is recreated; at start it refuses an intermediate the root did not sign,
   and makes its server certificate from the new one.

4. **Check:** `flotilla host list` shows every host connected again. Each host's current
   certificate, issued by the old intermediate, keeps working until it renews (at most 20 days),
   and the renewal comes from the new one.

5. **Destroy the old intermediate's key.**

The agents trust the root, not an intermediate. The old intermediate therefore stays usable for a
server certificate under flotillad's name until its own certificate expires, and nothing on the
hosts can revoke it. Destroying its key is what ends that. A leaked one needs the root replaced.

## Replace the root

When: the root's key or an intermediate's key may have leaked, or the root nears its 10 years.
Every host needs reinstalling, over SSH.

1. **Freeze:** `flotilla autonomy freeze`. While frozen every routine runs at shadow and no case
   can be approved.
2. **Make a new root and intermediate**, offline, as at first setup:

   ```bash
   flotilla ca root ~/flotilla-keys/root-2
   flotilla ca intermediate ~/flotilla-keys/root-2 ~/flotilla-keys/inter-2
   ```

3. **Put them in place:** the three files from `inter-2` (`intermediate.crt`, `intermediate.key`,
   `root.crt`) replace those in `$FLOTILLA_CONFIG/host-ca/`. Deploy. From now on every host is
   refused: flotillad no longer trusts their certificates, and they no longer trust flotillad's.
4. **Reinstall each host** with the new root and a new token:

   ```bash
   deploy/host/install.sh remove HOST
   flotilla host enrolment HOST          # a single-use token, 15 minutes: into a mode-600 file
   deploy/host/install.sh install HOST --ref <version> --os <os> --arch <arch> --config <its config> \
     --root ~/flotilla-keys/inter-2/root.crt --addr <flotillad's endpoint> --server-name <its name> \
     --token-file <the token file>
   ```

   `remove` deletes the host's key and certificate and keeps its audit log, so the host enrols with
   a new key.

5. **Check** `flotilla host list`, then `flotilla autonomy unfreeze --reason "..."`.
6. **Keep the new root's key offline; destroy the old root and intermediate keys.**

## A host's certificate

**Normally nothing to do.** The agent renews its certificate over its connection when less than
10 days of its 30 are left. flotillad raises a `certificate_expiring` host alert when less than 7
days are left, so the alert means renewal has not worked for three days.

**The certificate expired** (the host was off for more than 30 days, or renewal kept failing): the
agent cannot connect. Enrol it again; it keeps its key:

```bash
flotilla host enrolment HOST                      # the token into a mode-600 file, TOKEN
scp -p TOKEN HOST:/tmp/flotilla-token             # -p keeps mode 600
ssh -t HOST 'sudo -u _flotilla -H env -i PATH=/usr/bin:/bin /usr/local/libexec/flotilla-host/flotilla-host enrol \
  -addr <flotillad endpoint> -server-name <its name> < /tmp/flotilla-token; rm -f /tmp/flotilla-token'
rm -f TOKEN
```

The token is staged as a file and read on stdin, never on a command line, the way `install.sh`
stages it; `-t` lets sudo ask for a password. The addresses are the ones in the host's
`/etc/flotilla-host/agent.env`. The running agent loads its certificate at every connection
attempt and retries with a backoff of at most 90 seconds, so it connects without a restart.

**The host or its key may be compromised:** `flotilla host revoke HOST`. flotillad cuts a connected
host off at its next heartbeat and refuses it from then on, without needing access to the host.
To bring it back after the investigation, reinstall it (`install.sh remove`, then `install` with a
new token), so that it enrols with a new key.

## If a key leaked

| Leaked | Do |
| --- | --- |
| A host's key | Revoke the host, as above. |
| The execution-signing key | **Freeze** first, then rotate the signing key without the two-key window: each host's config lists only the new key. Executions in flight are refused (their cases escalate). Then check every host's log against flotillad's record: `ssh -t HOST sudo cat /var/log/flotilla-host/audit.jsonl > HOST.jsonl`, then `flotilla host audit HOST --log HOST.jsonl` (AUDIT PASS or FAIL), and read the executions since the suspected time. |
| The intermediate | Replace the root. Rotating the intermediate is not enough, because agents keep trusting the old one for server certificates. A leaked intermediate cannot authenticate as an enrolled host: flotillad accepts only the exact certificate it issued. |
| The root | Replace the root. |

## Not covered

- None of this has been run end to end, on the mini or anywhere else. The first rotation of each
  kind is also its test.
- There is no expiry warning for the intermediate or the root. Check them with `openssl` at least
  once a year.
- `install.sh upgrade` always builds and installs a binary, even when only the config changes; pass
  the version the host already runs.
- After a signing-key rotation, nothing shows which key signed a given execution in `flotilla host
  list` or `flotilla case show`.

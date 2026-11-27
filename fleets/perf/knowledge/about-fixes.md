# Known CI fixes

`fixes.yaml` in this folder lists each fix under way for a slow step the watch ranks. The watch
reads it every run:

- the top three entries are named with their fix and owner, or "no known fix, no owner";
- an entry that drops out of the ranking while its fix is listed is reported as "fixed?", for a
  person to confirm and remove the fix.

Each fix is a flat map under `fixes:`:

- `item`: the row's key, the `id` the source gives it (required);
- `link`: the pull request or issue that fixes it;
- `owner`: the repository or session that owns the fix.

A person changes this file, as with an SOP; the watch never writes it. A change is a new version
of the fleet.

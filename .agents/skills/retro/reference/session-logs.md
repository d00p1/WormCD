# Session logs

OpenCode keeps session transcripts in a local SQLite database:

```
~/.local/share/opencode/opencode.db
```

Open it read-only; never write to it. A `-wal`/`-shm` pair may sit beside it.

## Find the session

```sh
sqlite3 ~/.local/share/opencode/opencode.db \
  "select id, title, directory, datetime(time_updated/1000,'unixepoch')
   from session_v2
   where directory like '%WormCD%'
   order by time_updated desc
   limit 10;"
```

The newest row for the current working directory is the session to review. Sub-agent runs are separate `session_v2` rows whose `parent_id` points at the parent session; read them when the main transcript delegates work.

`session` / `message` / `part` are the legacy tables and are empty for v2 sessions. Use `session_v2` and `session_message`.

## Read the transcript

```sh
sqlite3 ~/.local/share/opencode/opencode.db \
  "select type, seq, data from session_message
   where session_id='ses_...'
   order by seq;"
```

`type` is `user`, `assistant`, or `idle`. The `data` column is JSON. For assistant rows, `content` is an array of `reasoning`, `text`, and `tool` entries; each entry has its own `time.created` / `time.completed`.

## Extract tool calls

```sh
sqlite3 ~/.local/share/opencode/opencode.db \
  "select data from session_message
   where session_id='ses_...' and type='assistant';" \
| jq -r '.content[]?
   | select(.type=="tool")
   | .name + " :: "
     + ((.state.input.command // .state.input.description
         // .state.input.query // .state.input.prompt // "")
        | tostring | .[0:120])'
```

Count calls per tool by swapping the last stage for `group_by(.name)`. Tool inputs also carry `path`/`filePath` for file reads and edits.

## Timing and cost

- Session totals: `tokens_input`, `tokens_output`, `cost` on `session_v2`.
- Per-message wall time: `time.completed - time.created` inside each assistant row.
- Gaps between rows in `session_message.time_created` show where the run stalled.

## Practical notes

- Quote paths; the database lives outside the repo.
- If queries get heavy, copy the DB to `/tmp/opencode/` first (`sqlite3 ... ".backup ..."`).
- Do not paste raw transcripts into the retro output; cite `session_id`, message `seq`, and the tool call instead.

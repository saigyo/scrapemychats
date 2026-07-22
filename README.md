# scrapemychats

Export every conversation from a ChatGPT account to your own machine — text
**and** attached files — then browse it all in a beautiful, searchable,
offline HTML archive.

Built for accounts where ChatGPT's built-in "export data" button isn't
available (notably many **business/Team workspaces**), e.g. when you're
cancelling a subscription and don't want to lose years of conversations.

**Your data never leaves your machine.** The tool drives a real Chrome window
on your computer using your own logged-in session; nothing is sent anywhere
except to chatgpt.com itself.

## What you get

```
export/
├── viewer.html                     ← open this: search + browse everything
├── manifest.csv                    ← one row per chat: status, counts
├── errors.log                      ← anything that couldn't be fetched
└── 001_Some Chat Title_6a5f6592/
    ├── conversation.json           ← complete raw data (every message, tool call)
    ├── conversation.md             ← readable transcript
    └── files/                      ← attachments & generated images
```

The viewer is a single self-contained HTML file: instant full-text search
across all chats, optional Personal/Work categorisation with subcategories,
inline image thumbnails with a lightbox, keyboard navigation (`/` to search,
arrow keys to browse). No server, no internet, no dependencies — it works
from a USB stick.

## Requirements

- Python 3.10+
- Google Chrome (or Microsoft Edge with `--browser-channel msedge`)
- `pip install -r requirements.txt` (just Playwright — no `playwright install`
  needed; it drives your installed browser)

## Usage

```bash
# 1. Test with a few chats first (a Chrome window opens — log into ChatGPT
#    in it; if you have multiple workspaces, switch to the right one)
python export_chats.py --limit 3

# 2. Check the export/ folder looks right, then run the full export
python export_chats.py

# 3. Build the viewer and open it
python build_viewer.py
# → open export/viewer.html in your browser
```

The chat list is discovered automatically from your account and saved to
`chats.csv`. Alternatively, supply your own CSV (first column: chat URL,
second column: title) with `--csv mylist.csv`.

### Categories (optional)

```bash
cp categories.example.json categories.json
# edit the groups / subcategories / keywords to match your life
python build_viewer.py
```

Chats are auto-filed by keyword scoring (title matches weigh 4×). Anything
that matches nothing lands in **Unsorted** — skim that list, add keywords,
and re-run; regeneration takes seconds.

## Things to know

- **It's slow on purpose.** ChatGPT throttles bulk access ("You're making
  requests too quickly"). The script paces itself (~10–20 s per chat), backs
  off in escalating steps when throttled, and permanently slows down each
  time it happens. A 600-chat archive takes a few hours. Let it run; you can
  minimise the Chrome window but don't close it.
- **It's resumable.** Interrupt any time; re-running skips everything already
  exported and retries failures.
- **Some old files are gone forever.** OpenAI deletes uploaded file content
  server-side after a retention period. Those downloads fail with
  "file not found" — logged in `errors.log` — but the conversation text
  referencing them is still captured. No export method can recover them.
- **Headless doesn't work.** ChatGPT's bot protection blocks headless
  browsers; the visible Chrome window is required.
- **Privacy.** `export/`, `chats.csv`, and `browser_profile/` are
  git-ignored. `browser_profile/` contains your live ChatGPT login — never
  share or commit it, and delete it when you're done.

## How it works

Rather than scraping the page or asking for your credentials, the script
waits for ChatGPT's own frontend to request each conversation's JSON from
its backend and captures that response. Attachments are fetched through the
files API using the same session headers the page itself uses. This makes
the export complete (every message, tool call, and file reference) and
robust to UI redesigns.

## Disclaimer

For exporting **your own data** from **your own account**. Automating a
website may be subject to its terms of use — this tool deliberately behaves
like a (patient) human reader, but you use it at your own risk. Not
affiliated with OpenAI.

## License

MIT

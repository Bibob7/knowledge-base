#!/usr/bin/env python3
"""Usage report for the knowledge base: how much is stored, how much is global, and how
often the prompt hook actually injected something.

Read-only. Sources:
  - the SQLite database (DATABASE_URL from ~/.config/knowledge-base/config.env, or the
    default ~/.local/share/knowledge-base/knowledge.db)
  - the Claude Code transcripts in ~/.claude/projects/*/*.jsonl (hook injections and tool
    calls are recorded there)
  - the Claude Code auto memory folders ~/.claude/projects/*/memory/ (for comparison)

Usage: scripts/usage-report.py [--days N]
"""
import argparse
import collections
import datetime as dt
import json
import os
import re
import sqlite3
import sys
from pathlib import Path

HOME = Path.home()
PROJECTS = HOME / ".claude" / "projects"
MARKER = "Knowledge from the knowledge base"
ENTRY = re.compile(r"^- #(\d+) \[([^\]]+)\]", re.M)
KB_TOOL = re.compile(r"knowledge[-_]base.*__(\w+)$")


def db_path() -> Path:
    env = os.environ.get("DATABASE_URL")
    cfg = Path(os.environ.get("XDG_CONFIG_HOME", HOME / ".config")) / "knowledge-base" / "config.env"
    if not env and cfg.exists():
        for line in cfg.read_text().splitlines():
            if line.startswith("DATABASE_URL="):
                env = line.split("=", 1)[1].strip()
    if env and env.startswith("sqlite://"):
        return Path(env[len("sqlite://"):])
    if env:
        sys.exit(f"Only SQLite is supported by this report, DATABASE_URL is {env.split(':')[0]}://…")
    return Path(os.environ.get("XDG_DATA_HOME", HOME / ".local" / "share")) / "knowledge-base" / "knowledge.db"


def pct(a, b):
    return f"{a / b * 100:.0f} %" if b else "–"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--days", type=int, default=0, help="only look at the last N days (default: everything)")
    args = ap.parse_args()

    # --- Database --------------------------------------------------------------------------
    con = sqlite3.connect(f"file:{db_path()}?mode=ro", uri=True)
    docs = {}
    for id_, title, project, kind, status, tags, created in con.execute(
        "SELECT id, title, project, kind, status, tags, created_at FROM documents"
    ):
        docs[id_] = dict(title=title, project=project, kind=kind, status=status,
                         tags=json.loads(tags or "[]"), created=created[:10])
    active = [d for d in docs.values() if d["status"] == "active"]
    glob = [d for d in active if not d["project"]]
    auto = [d for d in active if "auto" in d["tags"]]
    first = min((d["created"] for d in docs.values()), default="–")
    # Without --days: only count sessions since the first entry, so time before the plugin
    # was installed does not dilute the injection rate.
    if args.days:
        since = dt.datetime.now(dt.timezone.utc) - dt.timedelta(days=args.days)
    else:
        since = dt.datetime.fromisoformat(first + "T00:00:00+00:00") if first != "–" else None

    # --- Transcripts -----------------------------------------------------------------------
    sessions = prompts = 0
    sess_with_inj = 0
    inj_events = 0
    injected = collections.Counter()
    cross = 0  # injections of global entries
    foreign_projects = collections.Counter()  # projects that received global entries
    tools = collections.Counter()
    for f in PROJECTS.glob("*/*.jsonl"):
        if since and dt.datetime.fromtimestamp(f.stat().st_mtime, dt.timezone.utc) < since:
            continue
        s_prompts = s_inj = 0
        cwd = None
        try:
            lines = f.open(encoding="utf-8")
        except OSError:
            continue
        for line in lines:
            if MARKER not in line and '"type":"user"' not in line and "knowledge" not in line:
                continue
            try:
                d = json.loads(line)
            except ValueError:
                continue
            ts = d.get("timestamp")
            if since and ts and dt.datetime.fromisoformat(ts.replace("Z", "+00:00")) < since:
                continue
            cwd = cwd or d.get("cwd")
            t = d.get("type")
            if t == "user" and not d.get("isMeta") and isinstance(d.get("message", {}).get("content"), str) \
                    and not d["message"]["content"].lstrip().startswith("<"):
                s_prompts += 1
            att = d.get("attachment") or {}
            if att.get("type") == "hook_additional_context":
                text = "\n".join(c for c in att.get("content", []) if isinstance(c, str))
                if MARKER in text:
                    s_inj += 1
                    for id_, scope in ENTRY.findall(text):
                        injected[int(id_)] += 1
                        if scope == "global":
                            cross += 1
                            foreign_projects[Path(cwd).name if cwd else "?"] += 1
            if t == "assistant":
                for c in d.get("message", {}).get("content", []) or []:
                    if isinstance(c, dict) and c.get("type") == "tool_use":
                        m = KB_TOOL.search(c.get("name", ""))
                        if m:
                            tools[m.group(1)] += 1
        if s_prompts:
            sessions += 1
            prompts += s_prompts
            inj_events += s_inj
            sess_with_inj += bool(s_inj)

    # --- Auto memory -----------------------------------------------------------------------
    am_files = am_lines = am_repos = 0
    am_list = []
    for mem in PROJECTS.glob("*/memory"):
        files = [p for p in mem.glob("*.md") if p.is_file()]
        if files:
            am_repos += 1
            am_files += len(files)
            n = sum(len(p.read_text(errors="ignore").splitlines()) for p in files)
            am_lines += n
            am_list.append((mem.parent.name, len(files), n))

    # --- Report ----------------------------------------------------------------------------
    total_inj = sum(injected.values())
    print(f"Knowledge base usage report  ({'last %d days' % args.days if args.days else 'since the first entry, ' + first})\n")
    print("STORED")
    print(f"  active entries        {len(active):5}   (superseded: {sum(d['status'] == 'superseded' for d in docs.values())})")
    print(f"  global                {len(glob):5}   {pct(len(glob), len(active))}")
    print(f"  project-specific      {len(active) - len(glob):5}   in {len({d['project'] for d in active if d['project']})} projects")
    print(f"  from session-end/auto {len(auto):5}   {pct(len(auto), len(active))}")
    print("\nINJECTED BY THE PROMPT HOOK")
    print(f"  sessions              {sessions:5}   with an injection: {sess_with_inj} ({pct(sess_with_inj, sessions)})")
    print(f"  user prompts          {prompts:5}   with an injection: {inj_events} ({pct(inj_events, prompts)})")
    print(f"  injected entries      {total_inj:5}   distinct: {len(injected)} of {len(active)} active ({pct(len(injected), len(active))})")
    print(f"  of which global       {cross:5}   {pct(cross, total_inj)}")
    if foreign_projects:
        print("  global entries reached " + ", ".join(f"{p} ({n})" for p, n in foreign_projects.most_common()))
    print("\n  most injected:")
    for id_, n in injected.most_common(8):
        d = docs.get(id_, {})
        scope = d.get("project") or "global"
        title = (d.get("title") or "(deleted)")[:70]
        print(f"   {n:3}×  #{id_:<4} [{scope}] {title}")
    print("\nTOOL CALLS BY CLAUDE")
    print("  " + (", ".join(f"{k} {v}" for k, v in tools.most_common()) or "none"))
    print("\nAUTO MEMORY (for comparison)")
    print(f"  {am_files} files, {am_lines} lines in {am_repos} repositories")
    for name, nf, nl in sorted(am_list, key=lambda x: -x[2]):
        print(f"   {nl:4} lines  {nf:2} files  {name}")
    print("\nNote: prompts under 20 characters and slash commands are never searched; a prompt")
    print("without a hit leaves no trace, so 'with an injection' is a lower bound of hook activity.")


if __name__ == "__main__":
    main()

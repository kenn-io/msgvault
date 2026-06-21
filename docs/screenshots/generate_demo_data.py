# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "faker>=28.0",
# ]
# ///
"""Generate a synthetic msgvault SQLite database for TUI demos.

The Parquet analytics cache is built separately by running
`msgvault build-cache --full-rebuild` against this database.
"""

import datetime
import hashlib
import os
import random
import sqlite3
import sys
from pathlib import Path

from faker import Faker

fake = Faker()
Faker.seed(42)
random.seed(42)

SCRIPT_DIR = Path(__file__).parent
OUTPUT_DIR = SCRIPT_DIR / "demo-data"
DB_PATH = OUTPUT_DIR / "msgvault.db"

# --- Configuration ---

ACCOUNTS = [
    {"email": "alex.chen@gmail.com", "name": "Alex Chen"},
    {"email": "jordan.miller@gmail.com", "name": "Jordan Miller"},
]

GMAIL_LABELS = [
    ("INBOX", "system"),
    ("SENT", "system"),
    ("STARRED", "system"),
    ("IMPORTANT", "system"),
    ("TRASH", "system"),
    ("DRAFT", "system"),
    ("SPAM", "system"),
    ("CATEGORY_PERSONAL", "system"),
    ("CATEGORY_SOCIAL", "system"),
    ("CATEGORY_PROMOTIONS", "system"),
    ("CATEGORY_UPDATES", "system"),
    ("CATEGORY_FORUMS", "system"),
    ("Projects", "user"),
    ("Receipts", "user"),
    ("Travel", "user"),
    ("Work", "user"),
]

DOMAINS = [
    "gmail.com", "yahoo.com", "outlook.com", "hey.com",
    "company.io", "university.edu", "nonprofit.org",
    "amazon.com", "github.com", "stripe.com", "slack.com",
    "notion.so", "linear.app", "vercel.com", "fly.io",
]

ATTACHMENT_TYPES = [
    ("report.pdf", "application/pdf"),
    ("photo.jpg", "image/jpeg"),
    ("screenshot.png", "image/png"),
    ("spreadsheet.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"),
    ("document.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
    ("archive.zip", "application/zip"),
    ("data.csv", "text/csv"),
    ("slides.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"),
    ("invoice.pdf", "application/pdf"),
    ("image.heic", "image/heic"),
]

DATE_START = datetime.datetime(2022, 1, 1, tzinfo=datetime.timezone.utc)
DATE_END = datetime.datetime(2024, 12, 31, tzinfo=datetime.timezone.utc)
TARGET_MESSAGES = 10000


def load_schema(conn: sqlite3.Connection) -> None:
    """Load schema from msgvault source repo if available, otherwise use embedded schema."""
    # Try to find the schema in a sibling msgvault repo
    schema_paths = [
        SCRIPT_DIR / "../../msgvault/internal/store/schema.sql",
        Path(os.environ.get("MSGVAULT_REPO", "")) / "internal/store/schema.sql",
    ]
    for p in schema_paths:
        resolved = p.resolve()
        if resolved.exists():
            print(f"  Using schema from {resolved}")
            conn.executescript(resolved.read_text())
            # Add tables that may not be in older schema versions
            conn.executescript("""
                CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
                    subject,
                    body_text,
                    content='',
                    content_rowid='id'
                );

                CREATE TABLE IF NOT EXISTS participant_identifiers (
                    id INTEGER PRIMARY KEY,
                    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
                    identifier_type TEXT NOT NULL,
                    identifier_value TEXT NOT NULL,
                    display_value TEXT,
                    is_primary BOOLEAN DEFAULT FALSE,
                    UNIQUE(participant_id, identifier_type, identifier_value)
                );

                CREATE TABLE IF NOT EXISTS conversation_participants (
                    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
                    participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
                    role TEXT NOT NULL DEFAULT 'member',
                    PRIMARY KEY (conversation_id, participant_id)
                );
            """)
            return

    print("  Warning: msgvault schema.sql not found, using embedded schema", file=sys.stderr)
    _create_embedded_schema(conn)


def _create_embedded_schema(conn: sqlite3.Connection) -> None:
    """Fallback embedded schema matching msgvault's current schema."""
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS sources (
            id INTEGER PRIMARY KEY,
            source_type TEXT NOT NULL,
            identifier TEXT NOT NULL,
            display_name TEXT,
            google_user_id TEXT UNIQUE,
            last_sync_at DATETIME,
            sync_cursor TEXT,
            sync_config JSON,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            UNIQUE(source_type, identifier)
        );

        CREATE TABLE IF NOT EXISTS participants (
            id INTEGER PRIMARY KEY,
            email_address TEXT,
            phone_number TEXT,
            display_name TEXT,
            domain TEXT,
            canonical_id TEXT,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
        );
        CREATE UNIQUE INDEX IF NOT EXISTS idx_participants_email ON participants(email_address)
            WHERE email_address IS NOT NULL;

        CREATE TABLE IF NOT EXISTS conversations (
            id INTEGER PRIMARY KEY,
            source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
            source_conversation_id TEXT,
            conversation_type TEXT NOT NULL,
            title TEXT,
            participant_count INTEGER DEFAULT 0,
            message_count INTEGER DEFAULT 0,
            unread_count INTEGER DEFAULT 0,
            last_message_at DATETIME,
            last_message_preview TEXT,
            metadata JSON,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            UNIQUE(source_id, source_conversation_id)
        );

        CREATE TABLE IF NOT EXISTS messages (
            id INTEGER PRIMARY KEY,
            conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
            source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
            source_message_id TEXT,
            message_type TEXT NOT NULL,
            sent_at DATETIME,
            received_at DATETIME,
            read_at DATETIME,
            delivered_at DATETIME,
            internal_date DATETIME,
            sender_id INTEGER REFERENCES participants(id),
            is_from_me BOOLEAN DEFAULT FALSE,
            subject TEXT,
            snippet TEXT,
            reply_to_message_id INTEGER REFERENCES messages(id),
            thread_position INTEGER,
            is_read BOOLEAN DEFAULT TRUE,
            is_delivered BOOLEAN,
            is_sent BOOLEAN DEFAULT TRUE,
            is_edited BOOLEAN DEFAULT FALSE,
            is_forwarded BOOLEAN DEFAULT FALSE,
            size_estimate INTEGER,
            has_attachments BOOLEAN DEFAULT FALSE,
            attachment_count INTEGER DEFAULT 0,
            deleted_at DATETIME,
            deleted_from_source_at DATETIME,
            delete_batch_id TEXT,
            archived_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            indexing_version INTEGER DEFAULT 1,
            metadata JSON,
            UNIQUE(source_id, source_message_id)
        );

        CREATE TABLE IF NOT EXISTS message_bodies (
            message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
            body_text TEXT,
            body_html TEXT
        );

        CREATE TABLE IF NOT EXISTS participant_identifiers (
            id INTEGER PRIMARY KEY,
            participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
            identifier_type TEXT NOT NULL,
            identifier_value TEXT NOT NULL,
            display_value TEXT,
            is_primary BOOLEAN DEFAULT FALSE,
            UNIQUE(participant_id, identifier_type, identifier_value)
        );

        CREATE TABLE IF NOT EXISTS conversation_participants (
            conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
            participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
            role TEXT NOT NULL DEFAULT 'member',
            PRIMARY KEY (conversation_id, participant_id)
        );

        CREATE TABLE IF NOT EXISTS message_recipients (
            id INTEGER PRIMARY KEY,
            message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
            participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
            recipient_type TEXT NOT NULL,
            display_name TEXT,
            UNIQUE(message_id, participant_id, recipient_type)
        );

        CREATE TABLE IF NOT EXISTS labels (
            id INTEGER PRIMARY KEY,
            source_id INTEGER REFERENCES sources(id) ON DELETE CASCADE,
            source_label_id TEXT,
            name TEXT NOT NULL,
            label_type TEXT,
            color TEXT,
            UNIQUE(source_id, name)
        );

        CREATE TABLE IF NOT EXISTS message_labels (
            message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
            label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
            PRIMARY KEY (message_id, label_id)
        );

        CREATE TABLE IF NOT EXISTS attachments (
            id INTEGER PRIMARY KEY,
            message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
            filename TEXT,
            mime_type TEXT,
            size INTEGER,
            content_hash TEXT,
            storage_path TEXT NOT NULL,
            media_type TEXT,
            width INTEGER,
            height INTEGER,
            duration_ms INTEGER,
            thumbnail_hash TEXT,
            thumbnail_path TEXT,
            source_attachment_id TEXT,
            attachment_metadata JSON,
            encryption_version INTEGER DEFAULT 0,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP
        );

        CREATE TABLE IF NOT EXISTS message_raw (
            message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
            raw_data BLOB NOT NULL,
            raw_format TEXT NOT NULL,
            compression TEXT DEFAULT 'zlib',
            encryption_version INTEGER DEFAULT 0
        );

        CREATE TABLE IF NOT EXISTS sync_runs (
            id INTEGER PRIMARY KEY,
            source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
            started_at DATETIME NOT NULL,
            completed_at DATETIME,
            status TEXT DEFAULT 'running',
            messages_processed INTEGER DEFAULT 0,
            messages_added INTEGER DEFAULT 0,
            messages_updated INTEGER DEFAULT 0,
            errors_count INTEGER DEFAULT 0,
            error_message TEXT,
            cursor_before TEXT,
            cursor_after TEXT
        );

        CREATE TABLE IF NOT EXISTS sync_checkpoints (
            source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
            checkpoint_type TEXT NOT NULL,
            checkpoint_value TEXT NOT NULL,
            updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            PRIMARY KEY (source_id, checkpoint_type)
        );

        -- Indexes
        CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, sent_at DESC);
        CREATE INDEX IF NOT EXISTS idx_messages_source ON messages(source_id);
        CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
        CREATE INDEX IF NOT EXISTS idx_messages_sent_at ON messages(sent_at DESC);
        CREATE INDEX IF NOT EXISTS idx_messages_deleted ON messages(source_id, deleted_from_source_at);

        CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
            subject,
            body_text,
            content='',
            content_rowid='id'
        );
    """)


def random_date() -> datetime.datetime:
    delta = DATE_END - DATE_START
    offset = random.randint(0, int(delta.total_seconds()))
    dt = DATE_START + datetime.timedelta(seconds=offset)
    # Bias toward business hours
    return dt.replace(hour=random.choices(range(24), weights=[1]*6 + [3]*12 + [2]*6)[0],
                      minute=random.randint(0, 59))


def get_or_create_participant(conn: sqlite3.Connection, email: str, name: str | None = None) -> int:
    row = conn.execute("SELECT id FROM participants WHERE email_address = ?", (email,)).fetchone()
    if row:
        pid = row[0]
    else:
        domain = email.split("@")[1] if "@" in email else ""
        conn.execute(
            "INSERT INTO participants (email_address, display_name, domain) VALUES (?, ?, ?)",
            (email, name or fake.name(), domain),
        )
        pid = conn.execute("SELECT last_insert_rowid()").fetchone()[0]
    conn.execute(
        "INSERT OR IGNORE INTO participant_identifiers (participant_id, identifier_type, identifier_value, display_value, is_primary) "
        "VALUES (?, 'email', ?, ?, TRUE)",
        (pid, email.lower(), email),
    )
    return pid


def generate_contacts(conn: sqlite3.Connection, count: int = 80) -> list[int]:
    """Pre-generate a pool of external contacts."""
    ids = []
    for _ in range(count):
        domain = random.choice(DOMAINS)
        email = f"{fake.user_name()}@{domain}"
        pid = get_or_create_participant(conn, email)
        ids.append(pid)
    return ids


DEMO_THREAD_MESSAGES = [
    {
        "from": "sarah.benson@company.io",
        "from_name": "Sarah Benson",
        "to_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Team,\n\n"
            "I've drafted the initial timeline for the Q3 infrastructure migration. "
            "We're looking at moving the primary database cluster to the new region "
            "starting July 15th, with a two-week buffer for testing.\n\n"
            "Key milestones:\n"
            "- July 1-14: Provision new cluster and run parallel writes\n"
            "- July 15: Cut over read traffic\n"
            "- July 22: Cut over write traffic\n"
            "- July 29: Decommission old cluster\n\n"
            "Let me know if this works with everyone's schedules."
        ),
        "date": datetime.datetime(2024, 6, 10, 9, 15, tzinfo=datetime.timezone.utc),
    },
    {
        "from_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Thanks Sarah. The timeline looks reasonable but I'm worried about the "
            "parallel writes phase. Last time we tried that with the analytics cluster "
            "we hit replication lag issues that took three days to sort out.\n\n"
            "Can we schedule a 30-minute deep dive on the replication strategy before "
            "we commit to the July 1st start date?"
        ),
        "date": datetime.datetime(2024, 6, 10, 10, 2, tzinfo=datetime.timezone.utc),
    },
    {
        "from": "sarah.benson@company.io",
        "from_name": "Sarah Benson",
        "to_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Good point. The analytics cluster was a different situation though, we were "
            "running async replication. For this migration I'm planning synchronous "
            "replication with a dedicated 10Gbps link between regions.\n\n"
            "But yes, let's do the deep dive. How about Thursday at 2pm?"
        ),
        "date": datetime.datetime(2024, 6, 10, 10, 45, tzinfo=datetime.timezone.utc),
    },
    {
        "from": "marcus.wright@company.io",
        "from_name": "Marcus Wright",
        "to_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Thursday 2pm works for me. I'll bring the latency benchmarks from "
            "the staging environment. We've been running synthetic load tests "
            "and the p99 looks promising, under 12ms cross-region.\n\n"
            "One concern: do we have a rollback plan if the cut-over fails? "
            "The last outage cost us about $40K in SLA credits."
        ),
        "date": datetime.datetime(2024, 6, 10, 11, 30, tzinfo=datetime.timezone.utc),
    },
    {
        "from_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "12ms p99 is great. That's well within our latency budget.\n\n"
            "For rollback, I think we need a blue-green approach. Keep the old "
            "cluster warm for at least 48 hours after cut-over. We can use the "
            "load balancer to flip traffic back in under 60 seconds if needed.\n\n"
            "I'll draft the rollback runbook before Thursday."
        ),
        "date": datetime.datetime(2024, 6, 10, 13, 5, tzinfo=datetime.timezone.utc),
    },
    {
        "from": "sarah.benson@company.io",
        "from_name": "Sarah Benson",
        "to_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Blue-green is exactly what I had in mind. I've already reserved the "
            "capacity so we won't be paying double for long.\n\n"
            "I'm also going to loop in DevOps to set up automated health checks "
            "that trigger the rollback automatically if error rates spike above 1%."
        ),
        "date": datetime.datetime(2024, 6, 10, 14, 20, tzinfo=datetime.timezone.utc),
    },
    {
        "from": "marcus.wright@company.io",
        "from_name": "Marcus Wright",
        "to_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Automated rollback is a great idea. Can we also add a circuit breaker "
            "on the write path? If the new cluster starts rejecting writes we should "
            "failover to the old cluster immediately rather than queuing.\n\n"
            "I've seen too many migrations where queued writes caused data "
            "consistency issues during recovery."
        ),
        "date": datetime.datetime(2024, 6, 10, 15, 10, tzinfo=datetime.timezone.utc),
    },
    {
        "from_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Agreed. Circuit breaker on writes is non-negotiable. I'll add it to "
            "the requirements doc.\n\n"
            "So to summarize what we need before the Thursday meeting:\n"
            "1. Rollback runbook (me)\n"
            "2. Latency benchmarks from staging (Marcus)\n"
            "3. Automated health check design (Sarah + DevOps)\n"
            "4. Circuit breaker spec (me)\n\n"
            "Anything else?"
        ),
        "date": datetime.datetime(2024, 6, 10, 15, 45, tzinfo=datetime.timezone.utc),
    },
    {
        "from": "sarah.benson@company.io",
        "from_name": "Sarah Benson",
        "to_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "One more thing: we should notify the on-call team about the migration "
            "window. Last time they weren't in the loop and nearly triggered an "
            "incident response when they saw the traffic shift.\n\n"
            "I'll send out the communication plan tomorrow. See everyone Thursday!"
        ),
        "date": datetime.datetime(2024, 6, 10, 16, 30, tzinfo=datetime.timezone.utc),
    },
    {
        "from": "marcus.wright@company.io",
        "from_name": "Marcus Wright",
        "to_me": True,
        "subject": "Re: Q3 Infrastructure Migration Plan",
        "body": (
            "Good call on the on-call notification. I'll also update the runbook "
            "wiki with the new architecture diagram so the overnight team knows "
            "what to expect.\n\n"
            "See you all Thursday. This is shaping up to be a solid plan."
        ),
        "date": datetime.datetime(2024, 6, 11, 8, 15, tzinfo=datetime.timezone.utc),
    },
]


def create_demo_thread(
    conn: sqlite3.Connection,
    source_id: int,
    account_pid: int,
    label_map: dict[tuple[int, str], int],
) -> None:
    """Create a curated 10-message email thread for the thread view screenshot."""
    # Create participants for the thread
    sarah_pid = get_or_create_participant(conn, "sarah.benson@company.io", "Sarah Benson")
    marcus_pid = get_or_create_participant(conn, "marcus.wright@company.io", "Marcus Wright")

    # Create conversation
    conv_id_str = "thread_demo_infra"
    conn.execute(
        "INSERT INTO conversations (source_id, source_conversation_id, conversation_type, "
        "title, message_count, last_message_at) VALUES (?, ?, 'email_thread', ?, ?, ?)",
        (source_id, conv_id_str, "Re: Q3 Infrastructure Migration Plan",
         len(DEMO_THREAD_MESSAGES),
         DEMO_THREAD_MESSAGES[-1]["date"].isoformat()),
    )
    conv_id = conn.execute("SELECT last_insert_rowid()").fetchone()[0]

    prev_msg_id = None
    for pos, msg in enumerate(DEMO_THREAD_MESSAGES):
        is_from_me = msg.get("from_me", False)

        if is_from_me:
            sender_id = account_pid
            recipient_ids = [sarah_pid, marcus_pid]
        else:
            sender_id = get_or_create_participant(conn, msg["from"], msg.get("from_name"))
            recipient_ids = [account_pid]
            # CC the other external participant
            other = marcus_pid if sender_id == sarah_pid else sarah_pid
            cc_ids = [other]

        source_msg_id = f"msg_demo_thread_{pos:02d}"
        sent_at = msg["date"].isoformat()

        conn.execute(
            "INSERT INTO messages (conversation_id, source_id, source_message_id, message_type, "
            "sent_at, internal_date, sender_id, is_from_me, subject, snippet, "
            "reply_to_message_id, thread_position, size_estimate, has_attachments, attachment_count) "
            "VALUES (?, ?, ?, 'email', ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)",
            (conv_id, source_id, source_msg_id, sent_at, sent_at,
             sender_id, int(is_from_me), msg["subject"], msg["body"][:100],
             prev_msg_id, pos, len(msg["body"])),
        )
        msg_id = conn.execute("SELECT last_insert_rowid()").fetchone()[0]
        prev_msg_id = msg_id

        conn.execute(
            "INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)",
            (msg_id, msg["body"]),
        )

        # from recipient
        conn.execute(
            "INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')",
            (msg_id, sender_id),
        )
        # to recipients
        for rid in recipient_ids:
            conn.execute(
                "INSERT OR IGNORE INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'to')",
                (msg_id, rid),
            )
        # cc
        if not is_from_me:
            for cid in cc_ids:
                conn.execute(
                    "INSERT OR IGNORE INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'cc')",
                    (msg_id, cid),
                )

        # Labels
        if is_from_me:
            lid = label_map.get((source_id, "SENT"))
        else:
            lid = label_map.get((source_id, "INBOX"))
        if lid:
            conn.execute("INSERT OR IGNORE INTO message_labels (message_id, label_id) VALUES (?, ?)", (msg_id, lid))

        # Also mark as IMPORTANT
        imp_lid = label_map.get((source_id, "IMPORTANT"))
        if imp_lid:
            conn.execute("INSERT OR IGNORE INTO message_labels (message_id, label_id) VALUES (?, ?)", (msg_id, imp_lid))


def populate(conn: sqlite3.Connection) -> None:
    # Create sources
    source_ids = []
    account_participant_ids = []
    for acct in ACCOUNTS:
        conn.execute(
            "INSERT INTO sources (source_type, identifier, display_name, sync_cursor, last_sync_at) "
            "VALUES ('gmail', ?, ?, '12345', datetime('now'))",
            (acct["email"], acct["name"]),
        )
        sid = conn.execute("SELECT last_insert_rowid()").fetchone()[0]
        source_ids.append(sid)
        pid = get_or_create_participant(conn, acct["email"], acct["name"])
        account_participant_ids.append(pid)

    # Create labels for each source
    label_map: dict[tuple[int, str], int] = {}
    for sid in source_ids:
        for label_name, label_type in GMAIL_LABELS:
            conn.execute(
                "INSERT INTO labels (source_id, source_label_id, name, label_type) VALUES (?, ?, ?, ?)",
                (sid, label_name, label_name, label_type),
            )
            lid = conn.execute("SELECT last_insert_rowid()").fetchone()[0]
            label_map[(sid, label_name)] = lid

    # Generate contacts
    contact_ids = generate_contacts(conn)

    # Generate messages
    conv_counter = 0
    for i in range(TARGET_MESSAGES):
        source_idx = random.randint(0, len(ACCOUNTS) - 1)
        sid = source_ids[source_idx]
        account_pid = account_participant_ids[source_idx]

        sent_at = random_date()
        is_sent = random.random() < 0.25

        # Conversation (some messages share threads)
        if random.random() < 0.7 or conv_counter == 0:
            conv_counter += 1
            conn.execute(
                "INSERT INTO conversations (source_id, source_conversation_id, conversation_type, message_count, last_message_at) "
                "VALUES (?, ?, 'email_thread', 1, ?)",
                (sid, f"thread_{conv_counter:05d}", sent_at.isoformat()),
            )
            conv_id = conn.execute("SELECT last_insert_rowid()").fetchone()[0]
        else:
            # Add to existing conversation
            conn.execute(
                "UPDATE conversations SET message_count = message_count + 1, last_message_at = ? WHERE id = ?",
                (sent_at.isoformat(), conv_id),
            )

        if is_sent:
            sender_id = account_pid
            recipient_id = random.choice(contact_ids)
        else:
            sender_id = random.choice(contact_ids)
            recipient_id = account_pid

        subject = fake.sentence(nb_words=random.randint(3, 10)).rstrip(".")
        body = "\n\n".join(fake.paragraphs(nb=random.randint(1, 4)))
        snippet = body[:100]
        has_attach = random.random() < 0.05
        num_attach = random.randint(1, 3) if has_attach else 0
        size = random.randint(1000, 500000) if not has_attach else random.randint(10000, 500000)

        conn.execute(
            "INSERT INTO messages (conversation_id, source_id, source_message_id, message_type, "
            "sent_at, internal_date, sender_id, is_from_me, subject, snippet, "
            "size_estimate, has_attachments, attachment_count) "
            "VALUES (?, ?, ?, 'email', ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (conv_id, sid, f"msg_{i:06d}", sent_at.isoformat(), sent_at.isoformat(),
             sender_id, int(is_sent), subject, snippet, size, int(has_attach), num_attach),
        )
        msg_id = conn.execute("SELECT last_insert_rowid()").fetchone()[0]

        # Message body (separate table in new schema)
        conn.execute(
            "INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)",
            (msg_id, body),
        )

        # Recipients: 'from' row for the sender, 'to' row for the recipient
        conn.execute(
            "INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')",
            (msg_id, sender_id),
        )
        conn.execute(
            "INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'to')",
            (msg_id, recipient_id),
        )
        # Occasional CC
        if random.random() < 0.15:
            cc_id = random.choice(contact_ids)
            conn.execute(
                "INSERT OR IGNORE INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'cc')",
                (msg_id, cc_id),
            )

        # Labels
        applied_labels = []
        if is_sent:
            applied_labels.append("SENT")
        else:
            applied_labels.append("INBOX")
            if random.random() < 0.15:
                applied_labels.append("STARRED")
            if random.random() < 0.3:
                applied_labels.append("IMPORTANT")
            # Category labels
            cat = random.choices(
                ["CATEGORY_PERSONAL", "CATEGORY_SOCIAL", "CATEGORY_PROMOTIONS",
                 "CATEGORY_UPDATES", "CATEGORY_FORUMS"],
                weights=[40, 15, 20, 15, 10],
            )[0]
            applied_labels.append(cat)
            # User labels
            if random.random() < 0.2:
                applied_labels.append(random.choice(["Projects", "Receipts", "Travel", "Work"]))

        for label_name in applied_labels:
            lid = label_map.get((sid, label_name))
            if lid:
                conn.execute(
                    "INSERT OR IGNORE INTO message_labels (message_id, label_id) VALUES (?, ?)",
                    (msg_id, lid),
                )

        # Attachments
        if has_attach:
            for _ in range(num_attach):
                fname, mtype = random.choice(ATTACHMENT_TYPES)
                asize = random.randint(5000, 300000)
                chash = hashlib.sha256(f"{msg_id}_{fname}_{random.random()}".encode()).hexdigest()
                spath = f"{chash[:2]}/{chash}"
                conn.execute(
                    "INSERT INTO attachments (message_id, filename, mime_type, size, content_hash, storage_path) "
                    "VALUES (?, ?, ?, ?, ?, ?)",
                    (msg_id, fname, mtype, asize, chash, spath),
                )

    # --- Curated email thread for screenshot demos ---
    create_demo_thread(conn, source_ids[0], account_participant_ids[0], label_map)

    # Populate conversation_participants from messages + recipients
    conn.execute("""
        INSERT OR IGNORE INTO conversation_participants (conversation_id, participant_id, role)
        SELECT DISTINCT m.conversation_id, m.sender_id, 'member'
        FROM messages m WHERE m.sender_id IS NOT NULL
    """)
    conn.execute("""
        INSERT OR IGNORE INTO conversation_participants (conversation_id, participant_id, role)
        SELECT DISTINCT m.conversation_id, mr.participant_id, 'member'
        FROM messages m
        JOIN message_recipients mr ON m.id = mr.message_id
    """)

    # Populate FTS from message_bodies
    conn.execute("""
        INSERT INTO messages_fts(rowid, subject, body_text)
        SELECT m.id, m.subject, mb.body_text
        FROM messages m
        LEFT JOIN message_bodies mb ON m.id = mb.message_id
    """)


def main() -> None:
    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

    if DB_PATH.exists():
        DB_PATH.unlink()
        print(f"Removed existing {DB_PATH}")

    # Clean any existing analytics cache
    analytics_dir = OUTPUT_DIR / "analytics"
    if analytics_dir.exists():
        import shutil
        shutil.rmtree(analytics_dir)
        print(f"Removed existing {analytics_dir}")

    conn = sqlite3.connect(str(DB_PATH))
    conn.execute("PRAGMA journal_mode=WAL")

    print("Creating schema...")
    load_schema(conn)

    print(f"Generating {TARGET_MESSAGES} messages across {len(ACCOUNTS)} accounts...")
    populate(conn)
    conn.commit()

    # Stats
    msg_count = conn.execute("SELECT COUNT(*) FROM messages").fetchone()[0]
    part_count = conn.execute("SELECT COUNT(*) FROM participants").fetchone()[0]
    conv_count = conn.execute("SELECT COUNT(*) FROM conversations").fetchone()[0]
    attach_count = conn.execute("SELECT COUNT(*) FROM attachments").fetchone()[0]
    print(f"  {msg_count} messages, {conv_count} conversations, {part_count} participants, {attach_count} attachments")

    conn.close()
    print(f"Done! Database: {DB_PATH}")
    print("Run 'msgvault build-cache --full-rebuild' to generate the Parquet analytics cache.")


if __name__ == "__main__":
    main()

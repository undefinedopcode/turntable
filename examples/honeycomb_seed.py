#!/usr/bin/env python3
"""Seed a Honeycomb dataset with synthetic spans so the turntable connector has
something to aggregate.

Sends a batch of events spread over the last ~2 hours (so the connector's default
time_range=7200 covers them) to Honeycomb's Batch API. Sending events auto-creates
the dataset and its columns.

Usage:
    export HONEYCOMB_API_KEY=hcaik_...      # an Ingest key (Send Events permission)
    python3 honeycomb_seed.py [--dataset turntable-demo] [--count 500] [--region us|eu]

The same key value can later be used for querying if it also has the
"Manage Queries and Columns" + "Run Queries" permissions (or use a separate
Configuration key for turntable's api_key).
"""
import argparse
import json
import os
import random
import sys
import urllib.request
from datetime import datetime, timedelta, timezone

SERVICES = ["api", "web", "worker", "billing", "auth"]
ENDPOINTS = {
    "api": ["/v1/orders", "/v1/users", "/v1/search", "/v1/checkout"],
    "web": ["/", "/dashboard", "/settings", "/login"],
    "worker": ["process_job", "send_email", "reindex"],
    "billing": ["charge", "refund", "invoice"],
    "auth": ["/token", "/verify", "/refresh"],
}
STATUSES = [200, 200, 200, 200, 201, 301, 400, 404, 500]


def make_event(now):
    svc = random.choice(SERVICES)
    # duration_ms: log-normal-ish so P95/AVG are interesting.
    dur = round(random.lognormvariate(3.2, 0.9), 2)  # ~ tens to hundreds of ms
    status = random.choice(STATUSES)
    ts = now - timedelta(seconds=random.randint(0, 7000))
    return {
        "time": ts.astimezone(timezone.utc).isoformat(),
        "data": {
            "service.name": svc,
            "name": random.choice(ENDPOINTS[svc]),
            "duration_ms": dur,
            "http.status_code": status,
            "error": status >= 500,
            "user.id": f"u{random.randint(1, 50)}",
        },
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dataset", default="turntable-demo")
    ap.add_argument("--count", type=int, default=500)
    ap.add_argument("--region", choices=["us", "eu"], default="us")
    ap.add_argument("--key", default=os.environ.get("HONEYCOMB_API_KEY", ""))
    args = ap.parse_args()

    if not args.key:
        sys.exit("set HONEYCOMB_API_KEY (an Ingest key) or pass --key")

    base = "https://api.honeycomb.io" if args.region == "us" else "https://api.eu1.honeycomb.io"
    url = f"{base}/1/batch/{args.dataset}"
    now = datetime.now(timezone.utc)
    events = [make_event(now) for _ in range(args.count)]

    # The Batch API accepts up to a few thousand events; chunk to be safe.
    sent = 0
    for i in range(0, len(events), 500):
        chunk = events[i : i + 500]
        body = json.dumps(chunk).encode()
        req = urllib.request.Request(
            url, data=body, method="POST",
            headers={"X-Honeycomb-Team": args.key, "Content-Type": "application/json"},
        )
        with urllib.request.urlopen(req) as resp:
            results = json.loads(resp.read())
            ok = sum(1 for r in results if r.get("status") == 202)
            sent += ok
    print(f"sent {sent}/{len(events)} events to dataset {args.dataset!r} ({args.region})")
    print("query it, e.g.:")
    print(f"  SELECT service.name, COUNT(*) AS n, AVG(duration_ms) AS avg_dur")
    print(f"    FROM {args.dataset} GROUP BY service.name ORDER BY n DESC")


if __name__ == "__main__":
    main()

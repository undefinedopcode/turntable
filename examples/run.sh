#!/usr/bin/env bash
# examples/run.sh — demonstrate octoparser with the example data files.
# Usage: ./examples/run.sh [QUERY_INDEX]
#   With no arguments, runs all demo queries.
#   With an index (1-10), runs only that query.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OCTOPARSER="go run $ROOT/cmd/octoparser"
CONFIG="$ROOT/examples/octoparser.yaml"

run() {
    local num="$1"
    local desc="$2"
    shift 2

    # If an index filter is provided, skip everything else.
    if [[ -n "${FILTER:-}" && "$num" != "$FILTER" ]]; then
        return 0
    fi

    echo "=== $num. $desc ==="
    $OCTOPARSER -c "$CONFIG" "$@"
    echo
}

FILTER=""
if [[ $# -gt 0 ]]; then
    FILTER="$1"
    if [[ ! "$FILTER" =~ ^[0-9]+$ || "$FILTER" -lt 1 || "$FILTER" -gt 13 ]]; then
        echo "Usage: $0 [1-13]" >&2
        exit 1
    fi
fi

run 1 "JSON customers" \
    'SELECT * FROM customers WHERE active = true LIMIT 3'

run 2 "CSV orders" \
    'SELECT order_id, customer_id, amount FROM orders WHERE status = "paid" ORDER BY amount DESC LIMIT 3'

run 3 "YAML products" \
    'SELECT id, sku, name, price FROM products WHERE in_stock > 100 ORDER BY price DESC LIMIT 3'

run 4 "Cross-source join + aggregate" \
    'SELECT c.region, COUNT(o.order_id) AS orders, SUM(o.amount) AS revenue FROM orders o JOIN customers c ON c.id = o.customer_id WHERE o.status = "paid" GROUP BY c.region ORDER BY revenue DESC'

run 5 "Three-source join" \
    'SELECT p.category, p.name AS product, COUNT(o.order_id) AS sold FROM orders o JOIN products p ON p.id = o.product_id JOIN customers c ON c.id = o.customer_id WHERE c.region = "west" GROUP BY p.category, p.name ORDER BY sold DESC'

run 6 "LEFT JOIN" \
    'SELECT c.name, COUNT(o.order_id) AS orders FROM customers c LEFT JOIN orders o ON c.id = o.customer_id GROUP BY c.name ORDER BY orders DESC LIMIT 5'

run 7 "JSON output" \
    -o json 'SELECT name, email FROM customers WHERE region = "west" LIMIT 2'

run 8 "LIKE + IN" \
    "SELECT name, region FROM customers WHERE name LIKE '%e%' AND region IN ('west', 'south') ORDER BY name"

run 9 "explain" \
    --explain 'SELECT name, region FROM customers WHERE region = "west"'

run 10 "SQL database (pushdown)" \
    'SELECT id, item, qty, price FROM inventory WHERE qty > 20 ORDER BY price DESC LIMIT 5'

run 11 "CASE WHEN expression" \
    'SELECT c.name, CASE WHEN c.active THEN "active" ELSE "inactive" END AS status FROM customers c LIMIT 4'

run 12 "EXTRACT + date functions" \
    'SELECT o.order_id, EXTRACT(MONTH FROM o.placed_at) AS month, STRFTIME("%Y-%m", o.placed_at) AS ym, DATE_TRUNC("month", o.placed_at) AS trunc FROM orders o LIMIT 3'

run 13 "FROM-less scratch + string functions" \
    'SELECT 1 + 1 AS two, LEFT("octoparser", 4) AS prefix, POSITION("parse" IN "octoparser") AS pos, INITCAP("hello world") AS shout'

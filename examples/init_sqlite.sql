-- examples/init_sqlite.sql — seed the SQLite demo database used by turntable.yaml.
-- Run: sqlite3 examples/data/inventory.db < examples/init_sqlite.sql

DROP TABLE IF EXISTS inventory;
CREATE TABLE inventory (
    id INTEGER PRIMARY KEY,
    item TEXT NOT NULL,
    category TEXT NOT NULL,
    qty INTEGER NOT NULL,
    price REAL NOT NULL
);

INSERT INTO inventory (id, item, category, qty, price) VALUES
(1, 'hammer', 'tools', 10, 12.99),
(2, 'nails', 'hardware', 1000, 0.05),
(3, 'saw', 'tools', 5, 24.50),
(4, 'tape measure', 'tools', 50, 3.25),
(5, 'screwdriver set', 'tools', 8, 18.00),
(6, 'wood glue', 'hardware', 120, 4.50),
(7, 'level', 'tools', 15, 11.75),
(8, 'sandpaper', 'hardware', 200, 0.75);

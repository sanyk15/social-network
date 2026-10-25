CREATE INDEX IF NOT EXISTS users_name_last_first
    ON users (last_name text_pattern_ops, first_name text_pattern_ops);

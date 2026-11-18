-- ДЗ-4: посты и дружба.
--
-- Дружба неориентированная и хранится одной строкой на пару: user_id всегда
-- меньше friend_id (CHECK). Так пара (1,2) и (2,1) не может существовать
-- дважды, а лента для любого из друзей выбирается по user_id = $1 OR friend_id = $1.
--
-- Пост удаляется мягко (deleted_at): идентификатор остаётся в кэше ленты, но в
-- выдачу не попадает. Жёсткое удаление сломало бы ленивую фильтрацию в кэше.

CREATE TABLE IF NOT EXISTS friends (
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    friend_id  BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, friend_id),
    CONSTRAINT friends_canonical_order CHECK (user_id < friend_id)
);

-- Обратное направление: «кто у меня в друзьях» по friend_id.
CREATE INDEX IF NOT EXISTS friends_friend_id_idx ON friends (friend_id);

CREATE TABLE IF NOT EXISTS posts (
    id         BIGSERIAL   PRIMARY KEY,
    author_id  BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    text       TEXT        NOT NULL CHECK (char_length(text) BETWEEN 1 AND 4096),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

-- Перестройка ленты из СУБД: последние N постов каждого автора.
-- Индекс частичный: удалённые посты в ленту не попадают и в индексе не нужны.
CREATE INDEX IF NOT EXISTS posts_author_live_idx
    ON posts (author_id, id DESC)
    WHERE deleted_at IS NULL;

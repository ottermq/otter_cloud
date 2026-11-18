-- Personal-data retention and erasure (#219), as the privacy notice
-- promises (docs/legal/privacidade.md §5).
--
-- Both are SECURITY DEFINER functions owned by otter_migrator, so the
-- roles that call them get exactly these deletes and updates and nothing
-- more: audit_events stays append-only for otter_api and otter_admin, and
-- neither may delete a session or edit an email directly.

-- purge_personal_data, run by the api on a schedule:
-- - a session 90 days after it ended (revoked, or expired: 7 days idle or
--   30 days old, identity.SessionIdleTimeout and SessionAbsoluteTimeout);
-- - an invite never used, 90 days after it expired (its email belongs to
--   someone who never signed up);
-- - an audit event's IP, user agent and any email in its details, once the
--   event is 12 months old. The event itself stays.
CREATE FUNCTION purge_personal_data(
    OUT sessions_deleted bigint, OUT invites_deleted bigint, OUT audit_scrubbed bigint)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    DELETE FROM sessions
    WHERE least(coalesce(revoked_at, 'infinity'),
                last_seen_at + interval '7 days',
                created_at + interval '30 days') < now() - interval '90 days';
    GET DIAGNOSTICS sessions_deleted = ROW_COUNT;

    DELETE FROM invites
    WHERE used_at IS NULL AND expires_at < now() - interval '90 days';
    GET DIAGNOSTICS invites_deleted = ROW_COUNT;

    UPDATE audit_events
    SET ip = NULL, user_agent = NULL, details = details - 'email'
    WHERE at < now() - interval '12 months'
      AND (ip IS NOT NULL OR user_agent IS NOT NULL OR details ? 'email');
    GET DIAGNOSTICS audit_scrubbed = ROW_COUNT;
END
$$;

-- erase_user, run by `otter admin user erase` on the person's request.
-- Refused while the account has a database not yet deleted (SQLSTATE
-- 55000): the operator deletes those first, through the normal path with
-- its final dumps. Refused for an operator. Then, in one transaction:
-- - the email (on the user, the account's name, and the user's invites)
--   becomes a tombstone that names no one, the password hash one that
--   matches no password, the user disabled and the account closed;
-- - the user's sessions and password-reset links are deleted;
-- - every audit event about the user or the account, or carrying the
--   email (a failed login typed with it, say), loses its IP, user agent and
--   email. The events stay, by user id, as the record of what happened.
CREATE FUNCTION erase_user(p_user uuid,
    OUT sessions_deleted bigint, OUT audit_scrubbed bigint)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    u record;
    tomb text;
BEGIN
    SELECT id, account_id, email, is_operator INTO u FROM users WHERE id = p_user FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no user %', p_user USING ERRCODE = 'no_data_found';
    END IF;
    IF u.is_operator THEN
        RAISE EXCEPTION 'user % is an operator', p_user USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF EXISTS (SELECT FROM instances WHERE account_id = u.account_id AND status <> 'deleted') THEN
        RAISE EXCEPTION 'the account still has databases that are not deleted'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;

    tomb := 'erased-' || u.id || '@invalid';
    UPDATE users SET email = tomb, password_hash = '$argon2id$erased', status = 'disabled'
    WHERE id = u.id;
    UPDATE accounts SET name = tomb, status = 'closed' WHERE id = u.account_id;
    UPDATE invites SET email = tomb WHERE user_id = u.id OR email = u.email;

    DELETE FROM sessions WHERE user_id = u.id;
    GET DIAGNOSTICS sessions_deleted = ROW_COUNT;
    DELETE FROM password_resets WHERE user_id = u.id;

    UPDATE audit_events
    SET ip = NULL, user_agent = NULL, details = details - 'email'
    WHERE (account_id = u.account_id
           OR actor = 'user:' || u.id
           OR (target_type = 'user' AND target_id = u.id::text)
           OR details->>'email' = u.email)
      AND (ip IS NOT NULL OR user_agent IS NOT NULL OR details ? 'email');
    GET DIAGNOSTICS audit_scrubbed = ROW_COUNT;
END
$$;

REVOKE ALL ON FUNCTION purge_personal_data(), erase_user(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION purge_personal_data() TO otter_api;
GRANT EXECUTE ON FUNCTION erase_user(uuid) TO otter_admin;

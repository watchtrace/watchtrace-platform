-- name: GetDatabaseTime :one
SELECT CURRENT_TIMESTAMP::timestamptz AS database_time;

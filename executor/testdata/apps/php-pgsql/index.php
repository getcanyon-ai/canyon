<?php
// A plain PHP app on Postgres through PDO pgsql, which links libpq.so.5: each GET / writes a row and answers the
// count, so an answer proves the extension loaded and reached the database (executor/e2e_test.go; #1734).
$url = parse_url(getenv('DATABASE_URL'));
parse_str($url['query'] ?? '', $query);
$dsn = sprintf('pgsql:host=%s;port=%d;dbname=%s;sslmode=%s', $url['host'], $url['port'] ?? 5432, ltrim($url['path'], '/'), $query['sslmode'] ?? 'prefer');
$db = new PDO($dsn, urldecode($url['user']), urldecode($url['pass']), [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
$db->exec('CREATE TABLE IF NOT EXISTS visits (id serial PRIMARY KEY)');
$db->exec('INSERT INTO visits DEFAULT VALUES');
header('Content-Type: application/json');
echo json_encode(['n' => (int) $db->query('SELECT count(*) FROM visits')->fetchColumn()]);

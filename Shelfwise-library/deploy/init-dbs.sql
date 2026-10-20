-- One Postgres server, but a SEPARATE database per service.
-- No service ever connects to another service's database.
CREATE DATABASE catalog_db;
CREATE DATABASE member_db;
CREATE DATABASE circulation_db;
CREATE DATABASE reservation_db;
CREATE DATABASE fine_db;
CREATE DATABASE notification_db;

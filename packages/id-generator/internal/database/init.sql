CREATE TABLE id_segment (
  biz_tag VARCHAR(64) PRIMARY KEY,
  max_id BIGINT,
  step INT
);

INSERT INTO id_segment VALUES ('order', 1000000, 10000);
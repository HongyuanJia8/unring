INSERT INTO demo_accounts VALUES (3, 'Inserted inside unring');
INSERT INTO demo_projects VALUES (30, 3, 'Inserted project');
UPDATE demo_accounts SET name = 'Other, updated inside unring' WHERE id = 2;
DELETE FROM demo_accounts WHERE id = 1;

SELECT 'inside unring' AS vantage,
       (SELECT count(*) FROM demo_accounts) AS accounts,
       (SELECT count(*) FROM demo_projects) AS projects;

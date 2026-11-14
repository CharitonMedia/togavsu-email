-- Published view filed_messages, read by other modules under
-- views:email.filed_messages:read.
CREATE VIEW filed_messages AS
  SELECT f.matter_id, f.message_id, m.subject, m.direction, m.sent_at, f.html_file_id, f.note_id
  FROM filings f JOIN messages m ON m.id = f.message_id;

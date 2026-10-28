ALTER TABLE `message_log` ADD `quote_id` text;--> statement-breakpoint
CREATE INDEX `message_log_message` ON `message_log` (`thread_id`,`message_id`);--> statement-breakpoint
CREATE INDEX `message_log_quote` ON `message_log` (`thread_id`,`quote_id`);
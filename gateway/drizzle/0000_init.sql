CREATE TABLE `admin` (
	`identity` text PRIMARY KEY NOT NULL,
	`display_name` text,
	`notify_thread` text,
	`added_by` text,
	`added_at` integer NOT NULL
);
--> statement-breakpoint
CREATE TABLE `admin_token` (
	`token_hash` text PRIMARY KEY NOT NULL,
	`created_at` integer NOT NULL,
	`expires_at` integer NOT NULL,
	`used_by` text,
	`used_at` integer
);
--> statement-breakpoint
CREATE TABLE `binding` (
	`channel_id` text PRIMARY KEY NOT NULL,
	`platform` text NOT NULL,
	`channel_name` text,
	`trigger` text NOT NULL,
	`prefix` text,
	`reply_mode` text NOT NULL,
	`interrupt` text NOT NULL,
	`created_by` text NOT NULL,
	`created_at` integer NOT NULL,
	CONSTRAINT "binding_trigger" CHECK("binding"."trigger" in ('mention', 'all', 'prefix')),
	CONSTRAINT "binding_reply_mode" CHECK("binding"."reply_mode" in ('final', 'each')),
	CONSTRAINT "binding_interrupt" CHECK("binding"."interrupt" in ('queue', 'steer')),
	CONSTRAINT "binding_prefix" CHECK("binding"."trigger" <> 'prefix' or ("binding"."prefix" is not null and "binding"."prefix" <> ''))
);
--> statement-breakpoint
CREATE TABLE `bridge_cursor` (
	`id` integer PRIMARY KEY NOT NULL,
	`acked_seq` integer NOT NULL,
	CONSTRAINT "bridge_cursor_single_row" CHECK("bridge_cursor"."id" = 1)
);
--> statement-breakpoint
CREATE TABLE `classifier_rule` (
	`pattern` text PRIMARY KEY NOT NULL,
	`action` text NOT NULL,
	`created_by` text NOT NULL,
	`created_at` integer NOT NULL,
	CONSTRAINT "classifier_rule_action" CHECK("classifier_rule"."action" in ('allow', 'deny'))
);
--> statement-breakpoint
CREATE TABLE `classifier_setting` (
	`id` integer PRIMARY KEY NOT NULL,
	`enabled` integer NOT NULL,
	`updated_by` text NOT NULL,
	`updated_at` integer NOT NULL,
	CONSTRAINT "classifier_setting_single_row" CHECK("classifier_setting"."id" = 1)
);
--> statement-breakpoint
CREATE TABLE `message_log` (
	`seq` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`direction` text NOT NULL,
	`kind` text NOT NULL,
	`thread_id` text NOT NULL,
	`channel_id` text NOT NULL,
	`message_id` text NOT NULL,
	`payload` text NOT NULL,
	`created_at` integer NOT NULL,
	CONSTRAINT "message_log_direction" CHECK("message_log"."direction" in ('in', 'out')),
	CONSTRAINT "message_log_kind" CHECK("message_log"."kind" in ('message', 'command', 'action', 'reaction'))
);
--> statement-breakpoint
CREATE INDEX `message_log_created_at` ON `message_log` (`created_at`);--> statement-breakpoint
CREATE INDEX `message_log_channel` ON `message_log` (`channel_id`,`seq`);--> statement-breakpoint
CREATE INDEX `message_log_thread` ON `message_log` (`thread_id`,`seq`);--> statement-breakpoint
CREATE TABLE `sdk_kv` (
	`key` text PRIMARY KEY NOT NULL,
	`value` text NOT NULL,
	`expires_at` integer
);
--> statement-breakpoint
CREATE INDEX `sdk_kv_expires_at` ON `sdk_kv` (`expires_at`);--> statement-breakpoint
CREATE TABLE `sdk_lists` (
	`key` text NOT NULL,
	`idx` integer NOT NULL,
	`value` text NOT NULL,
	`expires_at` integer,
	PRIMARY KEY(`key`, `idx`)
);
--> statement-breakpoint
CREATE INDEX `sdk_lists_expires_at` ON `sdk_lists` (`expires_at`);--> statement-breakpoint
CREATE TABLE `sdk_locks` (
	`thread_id` text PRIMARY KEY NOT NULL,
	`token` text NOT NULL,
	`expires_at` integer NOT NULL
);
--> statement-breakpoint
CREATE TABLE `sdk_queues` (
	`thread_id` text NOT NULL,
	`idx` integer NOT NULL,
	`entry` text NOT NULL,
	`expires_at` integer NOT NULL,
	PRIMARY KEY(`thread_id`, `idx`)
);
--> statement-breakpoint
CREATE INDEX `sdk_queues_expires_at` ON `sdk_queues` (`expires_at`);--> statement-breakpoint
CREATE TABLE `sdk_subscriptions` (
	`thread_id` text PRIMARY KEY NOT NULL
);

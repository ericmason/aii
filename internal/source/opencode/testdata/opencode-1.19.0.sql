PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE `message` (
          `id` text PRIMARY KEY,
          `session_id` text NOT NULL,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `data` text NOT NULL,
          CONSTRAINT `fk_message_session_id_session_id_fk` FOREIGN KEY (`session_id`) REFERENCES `session`(`id`) ON DELETE CASCADE
        );
INSERT INTO message VALUES('msg_1370a5c36001NTTmFS9NDGM9EE','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924788278,1791924790304,'{"role":"user","time":{"created":1791924788278},"agent":"build","model":{"providerID":"openrouter","modelID":"google/gemini-2.5-flash"},"summary":{"diffs":[]}}');
INSERT INTO message VALUES('msg_1370a5e4c001OptPdf7wHa25AU','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924788812,1791924790286,'{"parentID":"msg_1370a5c36001NTTmFS9NDGM9EE","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/work/lantern","root":"/"},"cost":0.0023663,"tokens":{"total":7785,"input":7771,"output":14,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"google/gemini-2.5-flash","providerID":"openrouter","time":{"created":1791924788812,"completed":1791924790285},"finish":"tool-calls"}');
INSERT INTO message VALUES('msg_1370a64110019HDp7nsQjYHqrz','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924790289,1791924791499,'{"parentID":"msg_1370a5c36001NTTmFS9NDGM9EE","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/work/lantern","root":"/"},"cost":0.00051267,"tokens":{"total":7938,"input":660,"output":39,"reasoning":0,"cache":{"write":0,"read":7239}},"modelID":"google/gemini-2.5-flash","providerID":"openrouter","time":{"created":1791924790289,"completed":1791924791498},"finish":"stop"}');
INSERT INTO message VALUES('msg_1370a6f5b001wsBvMz3not6gsU','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924793179,1791924795403,'{"role":"user","time":{"created":1791924793179},"agent":"build","model":{"providerID":"openrouter","modelID":"google/gemini-2.5-flash"},"summary":{"diffs":[]}}');
INSERT INTO message VALUES('msg_1370a70da001zPbdgtTrG9rHIp','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924793562,1791924795388,'{"parentID":"msg_1370a6f5b001wsBvMz3not6gsU","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/work/lantern","root":"/"},"cost":0.00054061,"tokens":{"total":8000,"input":720,"output":43,"reasoning":0,"cache":{"write":0,"read":7237}},"modelID":"google/gemini-2.5-flash","providerID":"openrouter","time":{"created":1791924793562,"completed":1791924795387},"finish":"tool-calls"}');
INSERT INTO message VALUES('msg_1370a7800001hROzMFIT08kzER','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924795392,1791924796527,'{"parentID":"msg_1370a6f5b001wsBvMz3not6gsU","role":"assistant","mode":"build","agent":"build","path":{"cwd":"/work/lantern","root":"/"},"cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"google/gemini-2.5-flash","providerID":"openrouter","time":{"created":1791924795392,"completed":1791924796527},"finish":"stop"}');
CREATE TABLE `part` (
          `id` text PRIMARY KEY,
          `message_id` text NOT NULL,
          `session_id` text NOT NULL,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `data` text NOT NULL,
          CONSTRAINT `fk_part_message_id_message_id_fk` FOREIGN KEY (`message_id`) REFERENCES `message`(`id`) ON DELETE CASCADE
        );
INSERT INTO part VALUES('prt_1370a5c4d001bBo2JESPIaWWZ6','msg_1370a5c36001NTTmFS9NDGM9EE','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924788799,1791924788801,'{"type":"text","text":"\"Read README.md in this directory and summarize it in two sentences.\""}');
INSERT INTO part VALUES('prt_1370a63d5001SnMCrrp8VCGHzz','msg_1370a5e4c001OptPdf7wHa25AU','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924790229,1791924790230,'{"type":"step-start"}');
INSERT INTO part VALUES('prt_1370a63da001ELRYQ6F73zbM4j','msg_1370a5e4c001OptPdf7wHa25AU','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924790234,1791924790274,'{"type":"tool","tool":"read","callID":"tool_read_wPnInBrNNtiReXfitvew","state":{"status":"completed","input":{"filePath":"README.md"},"output":"<path>/work/lantern/README.md</path>\n<type>file</type>\n<content>\n1: # Lantern\n2: \n3: Lantern is a tiny command-line tool that converts Markdown checklists into\n4: a progress report. It reads every `- [ ]` and `- [x]` item in the files you\n5: pass it and prints how many are done, grouped by heading.\n6: \n7: Run it with `lantern notes/*.md`.\n\n(End of file - total 7 lines)\n</content>","metadata":{"preview":"# Lantern\n\nLantern is a tiny command-line tool that converts Markdown checklists into\na progress report. It reads every `- [ ]` and `- [x]` item in the files you\npass it and prints how many are done, grouped by heading.\n\nRun it with `lantern notes/*.md`.","truncated":false,"loaded":[],"display":{"type":"file","path":"/work/lantern/README.md","text":"# Lantern\n\nLantern is a tiny command-line tool that converts Markdown checklists into\na progress report. It reads every `- [ ]` and `- [x]` item in the files you\npass it and prints how many are done, grouped by heading.\n\nRun it with `lantern notes/*.md`.","lineStart":1,"lineEnd":7,"totalLines":7,"truncated":false}},"title":"work/lantern/README.md","time":{"start":1791924790242,"end":1791924790273}},"metadata":{"openrouter":{"reasoning_details":[]}}}');
INSERT INTO part VALUES('prt_1370a6405001cGoLEyMzz7xuU0','msg_1370a5e4c001OptPdf7wHa25AU','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924790278,1791924790278,'{"reason":"tool-calls","type":"step-finish","tokens":{"total":7785,"input":7771,"output":14,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.0023663}');
INSERT INTO part VALUES('prt_1370a6807001dL3tdtXGIpMPmz','msg_1370a64110019HDp7nsQjYHqrz','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924791303,1791924791304,'{"type":"step-start"}');
INSERT INTO part VALUES('prt_1370a680b001jp2hIDM0FVwJ4F','msg_1370a64110019HDp7nsQjYHqrz','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924791308,1791924791489,'{"type":"text","text":"Lantern is a command-line tool that converts Markdown checklists into a progress report, counting completed and incomplete items grouped by heading. It can be run with a command like `lantern notes/*.md`.","time":{"start":1791924791308,"end":1791924791488}}');
INSERT INTO part VALUES('prt_1370a68c4001KhZlHGoAzdKL0f','msg_1370a64110019HDp7nsQjYHqrz','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924791492,1791924791494,'{"reason":"stop","type":"step-finish","tokens":{"total":7938,"input":660,"output":39,"reasoning":0,"cache":{"write":0,"read":7239}},"cost":0.00051267}');
INSERT INTO part VALUES('prt_1370a6f5d0015TUO9cwapcpSvI','msg_1370a6f5b001wsBvMz3not6gsU','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924793546,1791924793547,'{"type":"text","text":"\"Now create notes.txt in this directory containing three short bullet points based on that summary.\""}');
INSERT INTO part VALUES('prt_1370a77d2001aqkqn4IXd73Z4A','msg_1370a70da001zPbdgtTrG9rHIp','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924795346,1791924795348,'{"type":"step-start"}');
INSERT INTO part VALUES('prt_1370a77d7001YG23dCQPhmpT57','msg_1370a70da001zPbdgtTrG9rHIp','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924795351,1791924795375,'{"type":"tool","tool":"write","callID":"tool_write_sOznNcVHGXojVHnGkcrZ","state":{"status":"completed","input":{"content":"- Lantern converts Markdown checklists to progress reports.\n- It counts completed and incomplete items.\n- Items are grouped by heading.","filePath":"notes.txt"},"output":"Wrote file successfully.","metadata":{"diagnostics":{},"filepath":"/work/lantern/notes.txt","exists":false,"truncated":false},"title":"work/lantern/notes.txt","time":{"start":1791924795361,"end":1791924795374}},"metadata":{"openrouter":{"reasoning_details":[]}}}');
INSERT INTO part VALUES('prt_1370a77f2001duOJ86bLdDta7E','msg_1370a70da001zPbdgtTrG9rHIp','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924795378,1791924795379,'{"reason":"tool-calls","type":"step-finish","tokens":{"total":8000,"input":720,"output":43,"reasoning":0,"cache":{"write":0,"read":7237}},"cost":0.00054061}');
INSERT INTO part VALUES('prt_1370a7c62001l2UXwh14MOmHCb','msg_1370a7800001hROzMFIT08kzER','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924796514,1791924796516,'{"type":"step-start"}');
INSERT INTO part VALUES('prt_1370a7c67001ErkD0fgL7w02mt','msg_1370a7800001hROzMFIT08kzER','ses_ec8f5a3f3ffeeYIaPK56zZ3KW9',1791924796519,1791924796520,'{"reason":"stop","type":"step-finish","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}');
CREATE TABLE `session_message` (
          `id` text PRIMARY KEY,
          `session_id` text NOT NULL,
          `type` text NOT NULL,
          `seq` integer NOT NULL,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `data` text NOT NULL,
          CONSTRAINT `fk_session_message_session_id_session_id_fk` FOREIGN KEY (`session_id`) REFERENCES `session`(`id`) ON DELETE CASCADE
        );
CREATE TABLE `session` (
          `id` text PRIMARY KEY,
          `project_id` text NOT NULL,
          `workspace_id` text,
          `parent_id` text,
          `slug` text NOT NULL,
          `directory` text NOT NULL,
          `path` text,
          `title` text NOT NULL,
          `version` text NOT NULL,
          `share_url` text,
          `summary_additions` integer,
          `summary_deletions` integer,
          `summary_files` integer,
          `summary_diffs` text,
          `metadata` text,
          `cost` real DEFAULT 0 NOT NULL,
          `tokens_input` integer DEFAULT 0 NOT NULL,
          `tokens_output` integer DEFAULT 0 NOT NULL,
          `tokens_reasoning` integer DEFAULT 0 NOT NULL,
          `tokens_cache_read` integer DEFAULT 0 NOT NULL,
          `tokens_cache_write` integer DEFAULT 0 NOT NULL,
          `revert` text,
          `permission` text,
          `agent` text,
          `model` text,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `time_compacting` integer,
          `time_archived` integer,
          CONSTRAINT `fk_session_project_id_project_id_fk` FOREIGN KEY (`project_id`) REFERENCES `project`(`id`) ON DELETE CASCADE
        );
INSERT INTO session VALUES('ses_ec8f5a3f3ffeeYIaPK56zZ3KW9','global',NULL,NULL,'curious-garden','/work/lantern','work/lantern','New session - 2026-10-13T20:53:08.236Z','1.19.0',NULL,0,0,0,NULL,NULL,0.0034195799999999998,9151,96,0,14476,0,NULL,'[{"permission":"question","pattern":"*","action":"deny"},{"permission":"plan_enter","pattern":"*","action":"deny"},{"permission":"plan_exit","pattern":"*","action":"deny"}]','build','{"id":"google/gemini-2.5-flash","providerID":"openrouter","variant":"default"}',1791924788236,1791924796541,NULL,NULL);
COMMIT;

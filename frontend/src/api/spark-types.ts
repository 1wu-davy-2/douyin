/**
 * 火花(续火花)契约类型 — docs/HUOHUA_EXECUTION_PLAN.md §5。
 * 字段名与后端 JSON 一致(snake_case)。
 */

export type SparkAccountStatus = "idle" | "sending" | "login_required" | "cooldown" | "error";

export interface SparkAccount {
  id: number;
  unique_id: string;
  username: string;
  nickname: string;
  profile_name: string;
  enabled: boolean;
  status: SparkAccountStatus;
  last_error: string;
  last_friends_refresh_at: string;
  last_send_at: string;
  cooldown_until: string;
  failure_count_today: string;
  created_at: string;
  updated_at: string;
}

export interface SparkFriendToday {
  id: number;
  account_id: number;
  friend_key: string;
  display_name: string;
  selected: boolean;
  /** 今日状态:""=未发;"strong"=已确认;"failed[:分类]"=今日失败。 */
  today_state: string;
}

export interface SparkFriendUpdate {
  key: string;
  selected: boolean;
}

export type SparkRunMode = "manual" | "manual_failed" | "manual_unsent" | "scheduled";

export interface SparkSendRecord {
  id: number;
  account_id: number;
  account_label: string;
  friend_key: string;
  message: string;
  confirm_state: "strong" | "failed";
  category: string;
  detail: string;
  run_mode: SparkRunMode;
  sent_at: string;
  local_date: string;
}

export interface SparkDayStats {
  strong: number;
  weak: number;
  failed: number;
}

export interface SparkEngineStatus {
  ok: boolean;
  version?: number;
  task_running?: boolean;
  detail?: string;
}

export interface SparkOverview {
  engine: SparkEngineStatus;
  accounts: SparkAccount[];
  today: SparkDayStats;
  window: { enabled: boolean; in_window: boolean; now: string };
}

export interface SparkSendWindow {
  enabled: boolean;
  startHour: number;
  endHour: number;
  intervalMinutes: number;
}

export interface SparkSendStrategy {
  shuffleTargets: boolean;
  accountStartDelaySecondsMin: number;
  accountStartDelaySecondsMax: number;
  messageIntervalSecondsMin: number;
  messageIntervalSecondsMax: number;
}

export interface SparkFriendScan {
  maxScanSeconds: number;
  idleScanSeconds: number;
  scrollStepPx: number;
  scrollDelaySeconds: number;
}

export interface SparkFailurePause {
  attempts: number;
  cooldownMinutes: number;
}

export interface SparkSettings {
  messageTemplate: string;
  messageVariants: string[];
  hitokotoTypes: string[];
  sendWindow: SparkSendWindow;
  sendStrategy: SparkSendStrategy;
  friendScan: SparkFriendScan;
  accountFailurePause: SparkFailurePause;
}

export interface SparkLoginStatus {
  running: boolean;
  logged_in: boolean;
  unique_id: string;
  nickname: string;
}

export interface SparkLoginExport {
  unique_id: string;
  nickname: string;
  profile_name: string;
  cookie: string;
}

export interface SparkRecordsPage {
  items: SparkSendRecord[];
  next_cursor: number;
}

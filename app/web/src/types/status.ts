// Mirrors mail.Status in the Go backend. Keep the two in sync — the same
// shape goes out over MQTT (`<topic>/status`) and over SSE.
export interface Alert {
  rule: string;
  title: string;
  check?: string;
  description?: string;
  type: 'state' | 'count' | 'silence';
  topic: string;
  state: 'pending' | 'firing';
  value?: string;
  since: string;
  fired_at?: string;
}

export interface AlertEvent {
  kind: 'firing' | 'resolved' | 'reminder';
  at: string;
  alert: Alert;
  duration?: string;
}

export interface RuleInfo {
  name: string;
  description?: string;
  type: 'state' | 'count' | 'silence';
  topics: string[];
  summary: string;
  watching: number;
  firing: number;
  pending: number;
}

export interface MailStats {
  enabled: boolean;
  sent: number;
  queued: number;
  sent_last_hour: number;
  last_sent_at?: string;
  last_error?: string;
  last_error_at?: string;
}

export interface Status {
  online: boolean;
  updated_at: string;
  started_at: string;
  messages: number;
  rules: RuleInfo[];
  alerts: Alert[];
  history: AlertEvent[];
  mail: MailStats;
}

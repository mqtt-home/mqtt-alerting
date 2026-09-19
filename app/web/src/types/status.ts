// Mirrors mail.Status in the Go backend. Keep the two in sync — the same
// shape goes out over MQTT (`<topic>/status`) and over SSE.
export interface Status {
  online: boolean;
  updated_at: string;
  // TODO: device fields
}

import type { Status } from '@/types/status';

// In `pnpm run dev` the Vite server serves the UI on :5173 while the Go backend
// stays on :8080; in production both are the same origin.
export const API_BASE = import.meta.env.DEV ? 'http://localhost:8080/api' : '/api';

export async function fetchStatus(): Promise<Status> {
  const response = await fetch(`${API_BASE}/status`);
  if (!response.ok) throw new Error('Failed to fetch status');
  return response.json();
}

export async function sendCommand(action: string, body: Record<string, unknown> = {}): Promise<void> {
  const response = await fetch(`${API_BASE}/${action}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    // The backend passes the SMTP error through; show it rather than a generic one.
    const detail = await response.json().catch(() => null);
    throw new Error(detail?.message ?? `Failed to ${action}`);
  }
}

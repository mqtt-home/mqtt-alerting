import { useState } from 'react';
import { Sun, Moon, Wifi, WifiOff, Mail, BellRing, CheckCircle2, CircleHelp, Clock, AlertTriangle } from 'lucide-react';
import { useSSE } from '@/hooks/useSSE';
import { useTheme } from '@/contexts/ThemeContext';
import { sendCommand } from '@/lib/api';
import type { Alert, AlertEvent, MailStats, RuleInfo } from '@/types/status';

function ago(iso?: string): string {
  if (!iso) return '–';
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 172800) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

function time(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    weekday: 'short', day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit',
  });
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="space-y-2">
      <h2 className="text-sm font-medium uppercase tracking-wide text-muted-foreground">{title}</h2>
      {children}
    </section>
  );
}

function AlertRow({ alert }: { alert: Alert }) {
  const firing = alert.state === 'firing';
  return (
    <div className={`rounded-lg border p-3 ${firing ? 'border-red-500/50 bg-red-500/10' : 'border-amber-500/40 bg-amber-500/10'}`}>
      <div className="flex items-start gap-3">
        {firing
          ? <BellRing className="mt-0.5 h-5 w-5 shrink-0 text-red-500" />
          : <Clock className="mt-0.5 h-5 w-5 shrink-0 text-amber-500" />}
        <div className="min-w-0 flex-1">
          <div className="font-medium">{alert.title}</div>
          <div className="break-all font-mono text-xs text-muted-foreground">{alert.topic}</div>
          <div className="text-sm text-muted-foreground">
            {alert.rule}{alert.value ? <> · <span className="font-mono">{alert.value}</span></> : null}
          </div>
        </div>
        <div className="shrink-0 text-right text-xs text-muted-foreground">
          <div className={firing ? 'font-medium text-red-500' : 'font-medium text-amber-500'}>{alert.state}</div>
          <div>{ago(alert.fired_at ?? alert.since)}</div>
        </div>
      </div>
    </div>
  );
}

// How much of what a rule watches is fine - the answer to "0 firing, but is
// this rule looking at anything?".
function coverage(rule: RuleInfo): string {
  if (rule.blind) return 'coverage unknown';
  if (rule.watching === 0) return 'nothing seen yet';
  if (rule.firing + rule.pending === 0) return `all ${rule.watching} fine`;
  return `${rule.ok} of ${rule.watching} fine`;
}

function RuleRow({ rule }: { rule: RuleInfo }) {
  const fine = !rule.blind && rule.watching > 0 && rule.firing + rule.pending === 0;
  return (
    <div className="rounded-lg border bg-card p-3 text-card-foreground">
      <div className="flex items-baseline justify-between gap-3">
        <div className="flex items-center gap-2 font-medium">
          {fine
            ? <CheckCircle2 className="h-4 w-4 shrink-0 text-green-500" />
            : rule.firing > 0
              ? <BellRing className="h-4 w-4 shrink-0 text-red-500" />
              : rule.pending > 0
                ? <Clock className="h-4 w-4 shrink-0 text-amber-500" />
                : <CircleHelp className="h-4 w-4 shrink-0 text-muted-foreground" />}
          {rule.name}
        </div>
        <div className="shrink-0 text-xs text-muted-foreground">
          {rule.firing > 0 && <span className="mr-2 font-medium text-red-500">{rule.firing} firing</span>}
          {rule.pending > 0 && <span className="mr-2 font-medium text-amber-500">{rule.pending} pending</span>}
          <span className={fine ? 'text-green-500' : ''}>{coverage(rule)}</span> · {rule.type}
        </div>
      </div>
      {rule.description && <div className="text-sm text-muted-foreground">{rule.description}</div>}
      {rule.type !== 'promql' && <div className="mt-1 text-sm">{rule.summary}</div>}
      <div className="mt-1 break-all font-mono text-xs text-muted-foreground">
        {rule.type === 'promql' ? rule.summary : (rule.topics ?? []).join('  ')}
      </div>
    </div>
  );
}

const kindStyle: Record<AlertEvent['kind'], string> = {
  firing: 'text-red-500',
  reminder: 'text-amber-500',
  resolved: 'text-green-500',
};

function HistoryRow({ event }: { event: AlertEvent }) {
  return (
    // On a phone the title gets a line of its own; next to date, kind and rule
    // it was squeezed into a column a few characters wide.
    <div className="flex flex-wrap items-baseline gap-x-3 border-b py-1.5 text-sm last:border-b-0">
      <span className="w-32 shrink-0 text-xs text-muted-foreground">{time(event.at)}</span>
      <span className={`w-16 shrink-0 text-xs font-medium ${kindStyle[event.kind]}`}>{event.kind}</span>
      <span className="order-last min-w-0 basis-full text-xs sm:order-none sm:basis-0 sm:flex-1" title={event.alert.topic}>{event.alert.title}</span>
      <span className="ml-auto shrink-0 text-xs text-muted-foreground">{event.duration ?? event.alert.rule}</span>
    </div>
  );
}

function MailCard({ mail }: { mail: MailStats }) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

  const test = async () => {
    setBusy(true);
    setResult(null);
    try {
      await sendCommand('test');
      setResult({ ok: true, text: 'Test mail sent.' });
    } catch (e) {
      setResult({ ok: false, text: e instanceof Error ? e.message : 'Failed' });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="rounded-lg border bg-card p-3 text-card-foreground">
      <div className="flex items-center gap-3">
        <Mail className="h-5 w-5 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1 text-sm">
          {mail.enabled ? (
            <>
              <div>{mail.sent} sent · {mail.sent_last_hour} in the last hour{mail.queued > 0 ? ` · ${mail.queued} queued` : ''}</div>
              <div className="text-muted-foreground">last mail {ago(mail.last_sent_at)}</div>
            </>
          ) : (
            <div className="text-amber-500">Sending is disabled in the config — alerts are only logged.</div>
          )}
        </div>
        <button
          onClick={test}
          disabled={busy}
          className="touch-target shrink-0 rounded-md border px-3 text-sm hover:bg-accent disabled:opacity-50"
        >
          {busy ? 'Sending…' : 'Send test mail'}
        </button>
      </div>
      {mail.last_error && (
        <div className="mt-2 flex items-start gap-2 rounded-md bg-red-500/10 p-2 text-sm text-red-500">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
          <span className="break-all">{mail.last_error} ({ago(mail.last_error_at)})</span>
        </div>
      )}
      {result && (
        <div className={`mt-2 text-sm ${result.ok ? 'text-green-500' : 'text-red-500'}`}>{result.text}</div>
      )}
    </div>
  );
}

export function App() {
  const { status, isConnected, error, reconnect } = useSSE();
  const { theme, toggleTheme } = useTheme();

  const firing = (status?.alerts ?? []).filter(a => a.state === 'firing').length;

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/95 backdrop-blur">
        <div className="mx-auto flex max-w-3xl items-center justify-between p-4">
          <h1 className="text-lg font-semibold">Alerting</h1>
          <div className="flex items-center gap-2">
            <button
              onClick={reconnect}
              title={isConnected ? 'Connected' : (error ?? 'Disconnected')}
              className="touch-target flex items-center justify-center rounded-md text-muted-foreground hover:text-foreground"
            >
              {isConnected
                ? <Wifi className="h-5 w-5 text-green-500" />
                : <WifiOff className="h-5 w-5 text-red-500" />}
            </button>
            <button
              onClick={toggleTheme}
              title="Toggle theme"
              className="touch-target flex items-center justify-center rounded-md text-muted-foreground hover:text-foreground"
            >
              {theme === 'dark' ? <Sun className="h-5 w-5" /> : <Moon className="h-5 w-5" />}
            </button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-3xl space-y-6 p-4">
        {status === null ? (
          <div className="text-muted-foreground">Waiting for status…</div>
        ) : (
          <>
            <Section title={firing > 0 ? `${firing} firing` : 'Alerts'}>
              {(status.alerts ?? []).length === 0 ? (
                <div className="flex items-center gap-3 rounded-lg border border-green-500/40 bg-green-500/10 p-3">
                  <CheckCircle2 className="h-5 w-5 text-green-500" />
                  <span className="text-sm">All quiet. {status.messages.toLocaleString()} messages checked since {time(status.started_at)}.</span>
                </div>
              ) : (
                <div className="space-y-2">
                  {(status.alerts ?? []).map(a => <AlertRow key={`${a.rule}|${a.topic}`} alert={a} />)}
                </div>
              )}
            </Section>

            <Section title="Mail">
              <MailCard mail={status.mail} />
            </Section>

            <Section title={`Rules (${(status.rules ?? []).length})`}>
              <div className="space-y-2">
                {(status.rules ?? []).map(r => <RuleRow key={r.name} rule={r} />)}
              </div>
            </Section>

            <Section title="History">
              {(status.history ?? []).length === 0 ? (
                <div className="text-sm text-muted-foreground">Nothing has fired since the service started.</div>
              ) : (
                <div className="rounded-lg border bg-card px-3 py-1 text-card-foreground">
                  {(status.history ?? []).map((e, i) => <HistoryRow key={`${e.at}|${i}`} event={e} />)}
                </div>
              )}
            </Section>
          </>
        )}
      </main>
    </div>
  );
}

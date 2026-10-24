import { useState } from 'react';
import {
  AlertTriangle, Bell, BellRing, CheckCircle2, CircleHelp, Clock, Mail, Send, ShieldCheck,
} from 'lucide-react';
import { useSSE } from '@/hooks/useSSE';
import { sendCommand } from '@/lib/api';
import { LangSwitch, useI18n } from '@/lib/i18n';
import type { Alert, AlertEvent, MailStats, RuleInfo, Status } from '@/types/status';

// Everything from the backend is read defensively: one `null` where a list is
// expected once blanked the whole page (v0.4.0, "topics": null).
const list = <T,>(xs: T[] | null | undefined): T[] => (Array.isArray(xs) ? xs : []);

const tint = (c: string) => ({ ['--c' as string]: c }) as React.CSSProperties;

function SecTitle({ title, meta }: { title: string; meta?: string }) {
  return (
    <div className="sec-title">
      <h2>{title}</h2>
      {meta && <span className="r num">{meta}</span>}
    </div>
  );
}

function TopBar({ status, isConnected, error, reconnect }: {
  status: Status | null;
  isConnected: boolean;
  error: ReturnType<typeof useSSE>['error'];
  reconnect: () => void;
}) {
  const i18n = useI18n();
  const { t, num } = i18n;
  return (
    <header className="top-bar">
      <div className="mx-auto flex max-w-3xl items-center gap-3 px-safe py-3">
        <span className="ico"><Bell /></span>
        <div className="min-w-0 flex-1">
          <h1 className="app-tt">{t('title')}</h1>
          {isConnected ? (
            <div className="app-st">
              <span className="dot live" />
              <span className="num truncate">
                {t('conn.live')}{status ? ` · ${t('conn.checked', { n: num(status.messages) })}` : ''}
              </span>
            </div>
          ) : (
            <button type="button" className="app-st" onClick={reconnect} title={t('conn.reconnect')}>
              <span className={error ? 'dot bad' : 'dot idle'} />
              <span className="truncate">{t(error ?? 'conn.connecting')}</span>
            </button>
          )}
        </div>
        <LangSwitch i18n={i18n} />
      </div>
    </header>
  );
}

function Stats({ status }: { status: Status }) {
  const { t, num } = useI18n();
  const alerts = list(status.alerts);
  const rules = list(status.rules);
  const firing = alerts.filter(a => a.state === 'firing').length;
  const pending = alerts.length - firing;
  const watching = rules.reduce((n, r) => n + (r.watching ?? 0), 0);
  return (
    <div className="stats">
      <div className={`stat${firing > 0 ? ' alert' : ''}`}>
        <div className="k"><BellRing />{t('stat.firing')}</div>
        <div className="v">{num(firing)}</div>
      </div>
      <div className={`stat${pending > 0 ? ' warn' : ''}`}>
        <div className="k"><Clock />{t('stat.pending')}</div>
        <div className="v">{num(pending)}</div>
      </div>
      <div className="stat">
        <div className="k"><ShieldCheck />{t('stat.rules')}</div>
        <div className="v">{num(rules.length)}</div>
        <div className="s num">{t('stat.watched', { w: num(watching) })}</div>
      </div>
      <div className="stat">
        <div className="k"><Mail />{t('stat.mails')}</div>
        <div className="v">{num(status.mail?.sent)}</div>
        <div className="s num">{t('stat.mailsHour', { n: num(status.mail?.sent_last_hour) })}</div>
      </div>
    </div>
  );
}

function AlertRow({ alert }: { alert: Alert }) {
  const { t, dur } = useI18n();
  const firing = alert.state === 'firing';
  const c = firing ? 'var(--alert)' : 'var(--warn)';
  return (
    <div className="arow" style={tint(c)}>
      <span className="ico" style={tint(c)}>{firing ? <BellRing /> : <Clock />}</span>
      <div className="min-w-0 flex-1">
        <div className="t">{alert.title}</div>
        <div className="topic">{alert.topic}</div>
        <div className="d">
          {alert.rule}{alert.value ? <> · <span className="mono">{alert.value}</span></> : null}
        </div>
      </div>
      <div className="side">
        <span className={`chip ${firing ? 'alert' : 'warn'}`}>{t(firing ? 'state.firing' : 'state.pending')}</span>
        <span className="num">{t('alert.for', { d: dur(alert.fired_at ?? alert.since) })}</span>
      </div>
    </div>
  );
}

function Attention({ status }: { status: Status }) {
  const { t, num, when } = useI18n();
  // Firing first, then pending; each oldest first.
  const alerts = [...list(status.alerts)].sort((a, b) =>
    (a.state === b.state ? 0 : a.state === 'firing' ? -1 : 1) ||
    (a.fired_at ?? a.since ?? '').localeCompare(b.fired_at ?? b.since ?? ''));
  return (
    <section>
      <SecTitle title={t('attention')} />
      {alerts.length === 0 ? (
        <div className="arow ok">
          <span className="ico" style={tint('var(--ok)')}><CheckCircle2 /></span>
          <div className="min-w-0 flex-1">
            <div className="t">{t('attention.quiet')}</div>
            <div className="d num">{t('attention.quietSub', { n: num(status.messages), date: when(status.started_at) })}</div>
          </div>
        </div>
      ) : (
        <div className="space-y-2">
          {alerts.map(a => <AlertRow key={`${a.rule}|${a.topic}`} alert={a} />)}
        </div>
      )}
    </section>
  );
}

function MailCard({ mail }: { mail: MailStats | null | undefined }) {
  const { t, num, ago } = useI18n();
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const m: MailStats = mail ?? { enabled: false, sent: 0, queued: 0, sent_last_hour: 0 };

  const test = async () => {
    setBusy(true);
    setResult(null);
    try {
      await sendCommand('test');
      setResult({ ok: true, text: t('mail.sent') });
    } catch (e) {
      // The backend passes the SMTP error through; that stays as is.
      setResult({ ok: false, text: e instanceof Error ? e.message : t('mail.failed') });
    } finally {
      setBusy(false);
    }
  };

  return (
    <section>
      <SecTitle title={t('mail')} />
      <div className="card">
        <div className="card-b flex flex-wrap items-center gap-3" style={{ paddingTop: 14 }}>
          <span className="ico sm" style={tint('var(--ink-3)')}><Mail /></span>
          <div className="min-w-0 flex-1 text-sm" style={{ flexBasis: 200 }}>
            {m.enabled ? (
              <>
                <div className="num">
                  {t('mail.summary', { sent: num(m.sent), hour: num(m.sent_last_hour) })}
                  {m.queued > 0 && <span style={{ color: 'var(--warn)' }}> · {t('mail.queued', { n: num(m.queued) })}</span>}
                </div>
                <div className="text-muted-foreground">
                  {m.last_sent_at ? t('mail.last', { ago: ago(m.last_sent_at) }) : t('mail.never')}
                </div>
              </>
            ) : (
              <div style={{ color: 'var(--warn)' }}>{t('mail.disabled')}</div>
            )}
          </div>
          <button type="button" className="btn" onClick={test} disabled={busy} style={busy ? { opacity: .6 } : undefined}>
            <Send className="h-4 w-4" />
            {busy ? t('mail.sending') : t('mail.test')}
          </button>
        </div>
        {(m.last_error || result) && (
          <div className="card-b space-y-2" style={{ paddingTop: 0 }}>
            {m.last_error && (
              <div className="flex items-start gap-2 rounded-xl p-2.5 text-sm"
                   style={{ background: 'var(--alert-soft)', color: 'var(--alert)' }}>
                <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
                <span className="break-all">{m.last_error} ({ago(m.last_error_at)})</span>
              </div>
            )}
            {result && (
              <div className="text-sm" style={{ color: result.ok ? 'var(--ok)' : 'var(--alert)' }}>{result.text}</div>
            )}
          </div>
        )}
      </div>
    </section>
  );
}

function RuleRow({ rule }: { rule: RuleInfo }) {
  const { t, num } = useI18n();
  const watching = rule.watching ?? 0;
  const bad = (rule.firing ?? 0) + (rule.pending ?? 0);
  const fine = !rule.blind && watching > 0 && bad === 0;
  // How much of what a rule watches is fine - the answer to "0 firing, but is
  // this rule looking at anything?".
  const coverage = rule.blind ? t('cov.unknown')
    : watching === 0 ? t('cov.none')
      : bad === 0 ? t('cov.all', { n: num(watching) })
        : t('cov.some', { ok: num(rule.ok), n: num(watching) });
  const pct = !rule.blind && watching > 0 ? Math.round((100 * (rule.ok ?? 0)) / watching) : 0;
  const icon = fine ? <CheckCircle2 style={{ color: 'var(--ok)' }} />
    : rule.firing > 0 ? <BellRing style={{ color: 'var(--alert)' }} />
      : rule.pending > 0 ? <Clock style={{ color: 'var(--warn)' }} />
        : <CircleHelp style={{ color: 'var(--ink-4)' }} />;
  return (
    <div className="rrow">
      {icon}
      <div className="min-w-0">
        <div className="n">{rule.name}</div>
        <div className="c num">
          {rule.firing > 0 && <span style={{ color: 'var(--alert)', fontWeight: 550 }}>{t('rule.firing', { n: num(rule.firing) })} · </span>}
          {rule.pending > 0 && <span style={{ color: 'var(--warn)', fontWeight: 550 }}>{t('rule.pending', { n: num(rule.pending) })} · </span>}
          <span className={fine ? 'fine' : ''}>{coverage}</span>
        </div>
        {rule.description && <div className="desc">{rule.description}</div>}
        {rule.type !== 'promql' && rule.summary && <div className="desc">{rule.summary}</div>}
        <div className="q">{rule.type === 'promql' ? rule.summary : list(rule.topics).join('  ')}</div>
      </div>
      <div className="meter"><i style={{ width: `${pct}%` }} /></div>
      <span className="x">{rule.type}</span>
    </div>
  );
}

function Rules({ rules }: { rules: RuleInfo[] }) {
  const { t, num } = useI18n();
  const watching = rules.reduce((n, r) => n + (r.watching ?? 0), 0);
  return (
    <section>
      <SecTitle title={t('rules')} meta={t('rules.meta', { n: num(rules.length), w: num(watching) })} />
      <div className="card" style={{ padding: '2px 16px' }}>
        {rules.length === 0
          ? <div className="py-3 text-sm text-muted-foreground">{t('rules.empty')}</div>
          : rules.map(r => <RuleRow key={r.name} rule={r} />)}
      </div>
    </section>
  );
}

const kindChip: Record<AlertEvent['kind'], string> = { firing: 'alert', reminder: 'warn', resolved: 'ok' };
const kindKey = { firing: 'kind.firing', reminder: 'kind.reminder', resolved: 'kind.resolved' } as const;

function History({ events }: { events: AlertEvent[] }) {
  const { t, when } = useI18n();
  return (
    <section>
      <SecTitle title={t('history')} meta={t('history.meta')} />
      {events.length === 0 ? (
        <div className="card card-b text-sm text-muted-foreground" style={{ paddingTop: 14 }}>{t('history.empty')}</div>
      ) : (
        <div className="card" style={{ padding: '2px 16px' }}>
          {events.map((e, i) => (
            <div key={`${e.at}|${i}`} className="hrow">
              <time className="num">{when(e.at)}</time>
              <span><span className={`chip ${kindChip[e.kind] ?? ''}`}>{kindKey[e.kind] ? t(kindKey[e.kind]) : e.kind}</span></span>
              <span className="tt min-w-0" title={e.alert?.topic}>{e.alert?.title}</span>
              <span className="x">{e.duration ?? e.alert?.rule}</span>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

export function App() {
  const { status, isConnected, error, reconnect } = useSSE();
  const { t } = useI18n();

  return (
    <div className="min-h-screen">
      <TopBar status={status} isConnected={isConnected} error={error} reconnect={reconnect} />
      <main className="mx-auto max-w-3xl space-y-7 px-safe pt-5"
            style={{ paddingBottom: 'calc(2.5rem + env(safe-area-inset-bottom))' }}>
        {status === null ? (
          <div className="text-muted-foreground">{t('waiting')}</div>
        ) : (
          <>
            <Stats status={status} />
            <Attention status={status} />
            <MailCard mail={status.mail} />
            <Rules rules={list(status.rules)} />
            <History events={list(status.history)} />
          </>
        )}
      </main>
    </div>
  );
}

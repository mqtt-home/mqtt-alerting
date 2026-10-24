import { useMemo } from 'react'
import { createI18n, type I18n as BaseI18n } from '@mqtt-home/ui/i18n'

export { LANGS, LangSwitch, resolveText, type Lang, type Text } from '@mqtt-home/ui/i18n'

// Only the UI chrome is translated; rule names, alert titles, descriptions and
// topics come from the config and stay as configured.
// German is the house language; English stays one tap away. Per device.
const dict = {
  de: {
    title: 'Alerting',
    'conn.live': 'live',
    'conn.connecting': 'Verbindung wird aufgebaut …',
    'conn.error': 'Verbindung unterbrochen',
    'conn.closed': 'Verbindung vom Server beendet',
    'conn.parse': 'Antwort des Servers unlesbar',
    'conn.reconnect': 'Neu verbinden',
    'conn.checked': '{n} Nachrichten geprüft',
    waiting: 'Warte auf den Status …',
    'stat.firing': 'Aktiv',
    'stat.pending': 'Ausstehend',
    'stat.rules': 'Regeln',
    'stat.mails': 'Mails gesendet',
    'stat.mailsHour': '{n} in der letzten Stunde',
    'stat.watched': '{w} überwacht',
    attention: 'Braucht Aufmerksamkeit',
    'attention.quiet': 'Alles ruhig',
    'attention.quietSub': '{n} Nachrichten geprüft seit {date}.',
    'state.firing': 'aktiv',
    'state.pending': 'ausstehend',
    'alert.for': 'seit {d}',
    mail: 'Mail',
    'mail.summary': '{sent} gesendet · {hour} in der letzten Stunde',
    'mail.queued': '{n} in der Warteschlange',
    'mail.last': 'Letzte Mail {ago}',
    'mail.never': 'Seit dem Start noch keine Mail gesendet',
    'mail.disabled': 'Versand ist in der Konfiguration abgeschaltet – Alarme werden nur protokolliert.',
    'mail.test': 'Test-Mail senden',
    'mail.sending': 'Wird gesendet …',
    'mail.sent': 'Test-Mail gesendet.',
    'mail.failed': 'Fehlgeschlagen',
    rules: 'Regeln',
    'rules.meta': '{n} Regeln · {w} überwacht',
    'rules.empty': 'Keine Regeln konfiguriert.',
    'cov.unknown': 'Abdeckung unbekannt',
    'cov.none': 'noch nichts gesehen',
    'cov.all': 'alle {n} in Ordnung',
    'cov.some': '{ok} von {n} in Ordnung',
    'rule.firing': '{n} aktiv',
    'rule.pending': '{n} ausstehend',
    history: 'Verlauf',
    'history.meta': 'seit dem Start',
    'history.empty': 'Seit dem Start hat nichts ausgelöst.',
    'kind.firing': 'ausgelöst',
    'kind.reminder': 'Erinnerung',
    'kind.resolved': 'behoben',
    'crash.title': 'Die Seite ist auf einen Fehler gestoßen',
    'crash.body': 'Die Alarmierung selbst läuft weiter – betroffen ist nur diese Seite. Der Rohstatus liegt unter',
    'crash.reload': 'Neu laden',
  },
  en: {
    title: 'Alerting',
    'conn.live': 'live',
    'conn.connecting': 'Connecting …',
    'conn.error': 'Connection lost',
    'conn.closed': 'Connection closed by server',
    'conn.parse': 'Could not read the server data',
    'conn.reconnect': 'Reconnect',
    'conn.checked': '{n} messages checked',
    waiting: 'Waiting for status …',
    'stat.firing': 'Firing',
    'stat.pending': 'Pending',
    'stat.rules': 'Rules',
    'stat.mails': 'Mails sent',
    'stat.mailsHour': '{n} in the last hour',
    'stat.watched': '{w} watched',
    attention: 'Needs attention',
    'attention.quiet': 'All quiet',
    'attention.quietSub': '{n} messages checked since {date}.',
    'state.firing': 'firing',
    'state.pending': 'pending',
    'alert.for': 'for {d}',
    mail: 'Mail',
    'mail.summary': '{sent} sent · {hour} in the last hour',
    'mail.queued': '{n} queued',
    'mail.last': 'Last mail {ago}',
    'mail.never': 'No mail sent since start',
    'mail.disabled': 'Sending is disabled in the config – alerts are only logged.',
    'mail.test': 'Send test mail',
    'mail.sending': 'Sending …',
    'mail.sent': 'Test mail sent.',
    'mail.failed': 'Failed',
    rules: 'Rules',
    'rules.meta': '{n} rules · {w} watched',
    'rules.empty': 'No rules configured.',
    'cov.unknown': 'coverage unknown',
    'cov.none': 'nothing seen yet',
    'cov.all': 'all {n} fine',
    'cov.some': '{ok} of {n} fine',
    'rule.firing': '{n} firing',
    'rule.pending': '{n} pending',
    history: 'History',
    'history.meta': 'since start',
    'history.empty': 'Nothing has fired since the service started.',
    'kind.firing': 'firing',
    'kind.reminder': 'reminder',
    'kind.resolved': 'resolved',
    'crash.title': 'The dashboard hit an error',
    'crash.body': 'Alerting itself keeps running – this is only the page. The raw status is at',
    'crash.reload': 'Reload',
  },
}

export type Key = keyof (typeof dict)['de']

export interface I18n extends Omit<BaseI18n<Key>, 'num'> {
  /** A number in the current locale (3.892.213 in German); null counts as 0. */
  num: (n: number | null | undefined, decimals?: number) => string
  /** How long since iso, as "22 min", "1 h 40 min", "3 d 4 h". */
  dur: (iso: string | null | undefined, now?: number) => string
  /** Weekday, date and 24h time: "Sa., 18.10., 11:49". */
  when: (iso: string | null | undefined) => string
}

const base = createI18n(dict)
export const I18nProvider = base.I18nProvider

const secs = (iso: string, now: number) => Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000))

export function useI18n(): I18n {
  const i = base.useI18n()
  return useMemo<I18n>(() => {
    const dur: I18n['dur'] = (iso, now = Date.now()) => {
      if (!iso) return '–'
      const s = secs(iso, now)
      if (s < 60) return `${s} s`
      const m = Math.floor(s / 60)
      if (m < 60) return `${m} min`
      const h = Math.floor(m / 60)
      if (h < 48) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`
      const d = Math.floor(h / 24)
      return h % 24 ? `${d} d ${h % 24} h` : `${d} d`
    }
    const dtf = new Intl.DateTimeFormat(i.locale, {
      weekday: 'short', day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
    })
    const when: I18n['when'] = (iso) => {
      if (!iso) return '–'
      const d = new Date(iso)
      return Number.isNaN(d.getTime()) ? '–' : dtf.format(d)
    }
    return {
      ...i,
      num: (n, decimals) => i.num(n ?? 0, decimals),
      // an unknown time shows as a dash, not as nothing
      ago: (iso, now) => (iso ? i.ago(iso, now) : '–'),
      dur,
      when,
    }
  }, [i])
}

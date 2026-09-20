import { Component, type ErrorInfo, type ReactNode } from 'react';

// Without this a single render error unmounts the whole tree and leaves a white
// page with nothing to go on. A dashboard for alerts must not fail silently.
export class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('UI crashed', error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="mx-auto max-w-3xl p-6">
        <h1 className="text-lg font-semibold text-red-500">The dashboard hit an error</h1>
        <p className="mt-2 text-sm text-muted-foreground">
          Alerting itself keeps running — this is only the page. The raw status is at{' '}
          <a className="underline" href="/api/status">/api/status</a>.
        </p>
        <pre className="mt-4 overflow-x-auto rounded-md border p-3 text-xs">{String(this.state.error?.stack ?? this.state.error)}</pre>
        <button className="mt-4 rounded-md border px-3 py-1.5 text-sm" onClick={() => location.reload()}>Reload</button>
      </div>
    );
  }
}

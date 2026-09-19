import { Sun, Moon, Wifi, WifiOff } from 'lucide-react';
import { useSSE } from '@/hooks/useSSE';
import { useTheme } from '@/contexts/ThemeContext';

export function App() {
  const { status, isConnected, error, reconnect } = useSSE();
  const { theme, toggleTheme } = useTheme();

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/95 backdrop-blur">
        <div className="mx-auto flex max-w-3xl items-center justify-between p-4">
          <h1 className="text-lg font-semibold">Mail Alerts</h1>
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

      <main className="mx-auto max-w-3xl p-4">
        {status === null ? (
          <div className="text-muted-foreground">Waiting for status…</div>
        ) : (
          <div className="rounded-lg border bg-card p-4 text-card-foreground">
            <pre className="overflow-x-auto text-sm">{JSON.stringify(status, null, 2)}</pre>
          </div>
        )}
      </main>
    </div>
  );
}

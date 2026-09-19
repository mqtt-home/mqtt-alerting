import { useEffect, useRef, useState, useCallback } from 'react';
import type { Status } from '@/types/status';
import { API_BASE } from '@/lib/api';

interface SSEHookReturn {
  status: Status | null;
  isConnected: boolean;
  error: string | null;
  reconnect: () => void;
}

// Live status over Server-Sent Events. EventSource reconnects on its own, but
// not after a server-side close — hence the explicit retry timer.
export function useSSE(): SSEHookReturn {
  const [status, setStatus] = useState<Status | null>(null);
  const [isConnected, setIsConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const eventSourceRef = useRef<EventSource | null>(null);
  const reconnectTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const cleanup = useCallback(() => {
    if (eventSourceRef.current) {
      eventSourceRef.current.close();
      eventSourceRef.current = null;
    }
    if (reconnectTimeoutRef.current) {
      clearTimeout(reconnectTimeoutRef.current);
      reconnectTimeoutRef.current = null;
    }
  }, []);

  const connect = useCallback(() => {
    cleanup();

    try {
      const eventSource = new EventSource(`${API_BASE}/events`);
      eventSourceRef.current = eventSource;

      eventSource.onopen = () => {
        setIsConnected(true);
        setError(null);
      };

      eventSource.onmessage = (event) => {
        try {
          setStatus(JSON.parse(event.data) as Status);
        } catch {
          setError('Failed to parse server data');
        }
      };

      eventSource.onerror = () => {
        setIsConnected(false);
        setError(eventSource.readyState === EventSource.CLOSED
          ? 'Connection closed by server'
          : 'Connection error');

        reconnectTimeoutRef.current = setTimeout(() => {
          if (eventSourceRef.current === eventSource) {
            connect();
          }
        }, 3000);
      };
    } catch {
      setError('Failed to connect to server');
    }
  }, [cleanup]);

  useEffect(() => {
    connect();
    return cleanup;
  }, [connect, cleanup]);

  const reconnect = useCallback(() => {
    setError(null);
    connect();
  }, [connect]);

  return { status, isConnected, error, reconnect };
}

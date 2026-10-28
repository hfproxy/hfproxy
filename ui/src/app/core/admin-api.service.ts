import { HttpClient, HttpErrorResponse, HttpHeaders } from '@angular/common/http';
import { inject, Injectable } from '@angular/core';
import { firstValueFrom } from 'rxjs';

@Injectable({ providedIn: 'root' })
export class AdminApiService {
  private readonly http = inject(HttpClient);

  get<T>(url: string): Promise<T> {
    return firstValueFrom(this.http.get<T>(url));
  }

  post<T = unknown>(url: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.post<T>(url, body, { headers: this.csrfHeaders() }));
  }

  put<T = unknown>(url: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.put<T>(url, body, { headers: this.csrfHeaders() }));
  }

  patch<T = unknown>(url: string, body: unknown): Promise<T> {
    return firstValueFrom(this.http.patch<T>(url, body, { headers: this.csrfHeaders() }));
  }

  delete<T = unknown>(url: string): Promise<T> {
    return firstValueFrom(this.http.delete<T>(url, { headers: this.csrfHeaders() }));
  }

  static errorMessage(error: unknown, fallback: string): string {
    if (error instanceof HttpErrorResponse) {
      const body = error.error as { detail?: string; title?: string } | string;
      if (typeof body === 'object' && body) return body.detail || body.title || fallback;
      if (typeof body === 'string' && body.trim()) {
        try {
          const parsed = JSON.parse(body) as { detail?: string; title?: string };
          if (parsed && typeof parsed === 'object') return parsed.detail || parsed.title || fallback;
        } catch {
          // Continue with the text/HTML error handling below.
        }
        if (body.trimStart().startsWith('<')) {
          const responseDocument = new DOMParser().parseFromString(body, 'text/html');
          const message = responseDocument
            .querySelector('[role="alert"], .error')
            ?.textContent?.trim();
          return message || fallback;
        }
        return (
          body
            .replace(/<[^>]*>/g, ' ')
            .trim()
            .slice(0, 180) || fallback
        );
      }
    }
    return error instanceof Error ? error.message : fallback;
  }

  private csrfHeaders(): HttpHeaders {
    return new HttpHeaders({ 'X-CSRF-Token': this.cookie('hfproxy_csrf') });
  }

  private cookie(name: string): string {
    const prefix = `${name}=`;
    return (
      document.cookie
        .split(';')
        .map((part) => part.trim())
        .find((part) => part.startsWith(prefix))
        ?.slice(prefix.length) ?? ''
    );
  }
}

import { HttpClient, HttpErrorResponse, HttpHeaders } from '@angular/common/http';
import { inject, Injectable } from '@angular/core';
import { firstValueFrom } from 'rxjs';

@Injectable({ providedIn: 'root' })
export class AdminApiService {
  private readonly http = inject(HttpClient);

  get<T>(url: string): Promise<T> {
    return firstValueFrom(this.http.get<T>(url));
  }

  async listAll<T>(url: string): Promise<T[]> {
    const items: T[] = [];
    const seenCursors = new Set<string>();
    let cursor = '';
    do {
      const params = new URLSearchParams({ limit: '200' });
      if (cursor) params.set('cursor', cursor);
      const separator = url.includes('?') ? '&' : '?';
      const page = await this.get<{ items: T[]; next_cursor?: string }>(
        `${url}${separator}${params.toString()}`,
      );
      items.push(...page.items);
      cursor = page.next_cursor ?? '';
      if (cursor && seenCursors.has(cursor)) {
        throw new Error('The API returned a repeated list cursor.');
      }
      if (cursor) seenCursors.add(cursor);
    } while (cursor);
    return items;
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

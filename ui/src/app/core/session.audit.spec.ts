import { TestBed } from '@angular/core/testing';
import { provideHttpClient, withInterceptors } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { provideRouter } from '@angular/router';
import { Component } from '@angular/core';
import { AdminApiService } from './admin-api.service';
import { AuthService } from './auth.service';
import { FeedbackService } from './feedback.service';
import { sessionInterceptor } from './session.interceptor';

@Component({ selector: 'app-login-stub', standalone: true, template: '' })
class LoginStub {}

const admin = { id: 'u1', username: 'admin', role: 'admin', authenticated: true };
const problem401 = {
  type: 'about:blank',
  title: 'Authentication required',
  status: 401,
  detail: 'Provide a valid hfproxy token or sign in.',
};

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

describe('AuditUI session handling', () => {
  let http: HttpTestingController;
  let auth: AuthService;
  let api: AdminApiService;
  let feedback: FeedbackService;

  beforeEach(async () => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(withInterceptors([sessionInterceptor])), provideHttpClientTesting(), provideRouter([{ path: 'login', component: LoginStub }])],
    });
    http = TestBed.inject(HttpTestingController);
    auth = TestBed.inject(AuthService);
    api = TestBed.inject(AdminApiService);
    feedback = TestBed.inject(FeedbackService);

    const init = auth.initialize();
    http.expectOne('/api/v1/me').flush(admin);
    await flushMicrotasks();
    http.expectOne('/api/v1/auth/providers').flush({ local: true, oidc: false, ldap: false });
    await init;
    expect(auth.principal()?.username).toBe('admin');
  });

  it('AuditUI: a 401 problem+json from the admin API (expired session) signs the user out of the UI', async () => {
    const pending = api.get('/api/v1/users').catch(() => undefined);
    http
      .expectOne('/api/v1/users')
      .flush(problem401, {
        status: 401,
        statusText: 'Unauthorized',
        headers: { 'Content-Type': 'application/problem+json' },
      });
    await pending;
    await flushMicrotasks();
    // Expected: session expiry is detected globally (interceptor) and the UI returns to the
    // login screen. Actual: the stale principal stays set and the admin shell keeps rendering.
    expect(auth.principal()).toBeNull();
  });

  it('AuditUI: sign-out reports a failure when the server refuses to revoke the session', async () => {
    document.cookie = 'hfproxy_csrf=stale-token; path=/';
    const pending = auth.signOut();
    http
      .expectOne('/logout')
      .flush(
        { title: 'CSRF validation failed', status: 403, detail: 'Reload the page and retry the action.' },
        { status: 403, statusText: 'Forbidden' },
      );
    await flushMicrotasks();
    http.match('/api/v1/auth/providers').forEach((r) => r.flush({ local: true, oidc: false, ldap: false }));
    await pending;
    // Expected: the user is told logout failed (the HttpOnly session cookie is still valid
    // server-side). Actual: every error is swallowed and the UI pretends to be signed out.
    expect(auth.error()).not.toBe('');
    expect(feedback.error()).toBe('Reload the page and retry the action.');
  });
});


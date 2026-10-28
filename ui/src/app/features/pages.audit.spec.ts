import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { provideRouter, Router } from '@angular/router';
import { Component } from '@angular/core';
import { OverviewPageComponent } from './overview/overview-page';
import { UsersPageComponent } from './users/users-page';
import { RepositoriesPageComponent } from './repositories/repositories-page';
import { AdminShellComponent } from '../layout/admin-shell';

@Component({ selector: 'app-blank', standalone: true, template: '' })
class BlankComponent {}

const problem401 = {
  type: 'about:blank',
  title: 'Authentication required',
  status: 401,
  detail: 'Sign in to view active downloads.',
};
const unauthorized = {
  status: 401,
  statusText: 'Unauthorized',
  headers: { 'Content-Type': 'application/problem+json' },
};

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

describe('AuditUI pages', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([
          { path: 'overview', component: BlankComponent },
          { path: 'cache', component: BlankComponent },
        ]),
      ],
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('AuditUI: overview stops polling /downloads/active once the session has expired (401)', async () => {
    vi.useFakeTimers();
    const page = TestBed.runInInjectionContext(() => new OverviewPageComponent());
    page.ngOnInit();
    http.expectOne('/api/v1/stats').flush(problem401, unauthorized);
    http.expectOne((request) => request.urlWithParams.split('?')[0] === '/api/v1/repos').flush(problem401, unauthorized);
    http.expectOne('/api/v1/downloads/active').flush(problem401, unauthorized);
    await flushMicrotasks();

    let extraPolls = 0;
    for (let i = 0; i < 5; i++) {
      vi.advanceTimersByTime(2_000);
      const polls = http.match('/api/v1/downloads/active');
      extraPolls += polls.length;
      polls.forEach((req) => req.flush(problem401, unauthorized));
      await flushMicrotasks();
    }
    page.ngOnDestroy();
    // Expected: 0 (stop polling / redirect to login). Actual: one 401 request every 2 seconds forever.
    expect(extraPolls).toBe(0);
  });

  it('AuditUI: users page does not submit the create form when required fields are empty / password < 12 chars', async () => {
    const fixture = TestBed.createComponent(UsersPageComponent);
    fixture.detectChanges();
    http.expectOne((request) => request.urlWithParams.split('?')[0] === '/api/v1/users').flush({ items: [] });
    await fixture.whenStable();
    fixture.detectChanges();

    const form: HTMLFormElement = fixture.nativeElement.querySelector('form');
    const password: HTMLInputElement = form.querySelector('input[name="password"]')!;
    password.value = 'short';
    password.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    form.dispatchEvent(new Event('submit'));
    await flushMicrotasks();

    // Template declares required + minlength="12", but FormsModule adds novalidate and
    // (ngSubmit) fires regardless of validity, so the invalid payload is POSTed anyway.
    const posts = http.match((req) => req.method === 'POST' && req.url === '/api/v1/users');
    expect(posts.length).toBe(0);
  });

  it('AuditUI: users page reverts the role select when saving a role change fails', async () => {
    const page = TestBed.runInInjectionContext(() => new UsersPageComponent()) as any;
    const user = {
      id: 'u2',
      username: 'bob',
      email: '',
      display_name: '',
      role: 'reader',
      status: 'active',
      created_at: '2026-01-01T00:00:00Z',
    };
    page.users.set([user]);
    page.setRole(user, 'admin'); // what [(ngModel)]="user.role" does when the select changes
    const pending = page.save(user);
    http
      .expectOne('/api/v1/users/u2')
      .flush({ title: 'Database error', status: 500, detail: 'Unable to update user.' }, {
        status: 500,
        statusText: 'Server Error',
      });
    await pending;
    // Expected: the list shows the role actually stored on the server ("reader").
    // Actual: the select keeps showing "admin" although the update was rejected.
    expect(page.users()[0].role).toBe('reader');
  });

  it('Issue80UI: repositories page reverts policy after a failed save', async () => {
    const page = TestBed.runInInjectionContext(() => new RepositoriesPageComponent()) as any;
    const loading = page.load();
    http.expectOne('/api/v1/repos?limit=200').flush({
      items: [{ id: 'r1', repo_id: 'org/model', access_policy: 'public' }],
    });
    await loading;
    const repository = page.repositories()[0];
    repository.access_policy = 'admin_only';
    const pending = page.savePolicy(repository);
    http
      .expectOne('/api/v1/repos/r1')
      .flush({ title: 'save failed', status: 500 }, { status: 500, statusText: 'Server Error' });
    await pending;
    expect(repository.access_policy).toBe('public');
    expect(page.repositories()[0].access_policy).toBe('public');
  });

  it('AuditUI: admin shell unsubscribes from router events when destroyed (sign-out)', async () => {
    const router = TestBed.inject(Router);
    await router.navigateByUrl('/overview');
    const fixture = TestBed.createComponent(AdminShellComponent);
    const shell = fixture.componentInstance as any;
    fixture.destroy(); // happens on every sign-out: app.html removes the router-outlet
    await router.navigateByUrl('/cache');
    // Expected: a destroyed component no longer reacts to router events ('/overview').
    // Actual: the constructor subscription is never torn down and keeps updating it.
    expect(shell.path()).toBe('/overview');
  });
});


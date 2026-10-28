import { Component, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { NavigationEnd, Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { filter } from 'rxjs';
import { AuthService } from '../core/auth.service';
import { FeedbackService } from '../core/feedback.service';
import { IconComponent } from '../shared/icon/icon';

interface NavItem {
  path: string;
  label: string;
  icon: string;
}

@Component({
  selector: 'app-admin-shell',
  standalone: true,
  imports: [RouterOutlet, RouterLink, RouterLinkActive, IconComponent],
  templateUrl: './admin-shell.html',
})
export class AdminShellComponent {
  protected readonly auth = inject(AuthService);
  protected readonly feedback = inject(FeedbackService);
  private readonly router = inject(Router);
  protected readonly path = signal('/overview');

  protected readonly navItems: NavItem[] = [
    { path: '/overview', label: 'Overview', icon: 'grid' },
    { path: '/repositories', label: 'Repositories', icon: 'box' },
    { path: '/cache', label: 'Cache', icon: 'database' },
    { path: '/users', label: 'Users', icon: 'users' },
    { path: '/groups', label: 'Groups', icon: 'layers' },
    { path: '/audit', label: 'Audit log', icon: 'shield' },
    { path: '/settings', label: 'Settings', icon: 'settings' },
  ];

  constructor() {
    this.router.events
      .pipe(filter((event) => event instanceof NavigationEnd))
      .pipe(takeUntilDestroyed())
      .subscribe(() => this.path.set(this.router.url));
    this.path.set(this.router.url);
  }

  protected get visibleNavItems(): NavItem[] {
    return this.auth.principal()?.role === 'admin' ? this.navItems : this.navItems.slice(0, 1);
  }

  protected get currentTitle(): string {
    const route = this.path();
    return this.navItems.find((item) => route.startsWith(item.path))?.label ?? 'Overview';
  }

  protected get initials(): string {
    const name = this.auth.principal()?.display_name || this.auth.principal()?.username || 'H';
    return name.slice(0, 1).toUpperCase();
  }

  protected signOut(): void {
    void this.auth.signOut();
  }
}

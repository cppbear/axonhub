import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';
import { RouteGuard } from '@/components/route-guard';
import ModelsManagement from '@/features/models';

const reviewScope = z.object({
  channels: z.boolean(),
  providers: z.array(z.string().trim().min(1)),
  ignorePrefix: z.boolean(),
});

function ProtectedModels() {
  const { catalogReview } = Route.useSearch();
  return (
    <RouteGuard requiredScopes={['read_channels']} scopeLevel="system">
      <RouteGuard requiredScopes={catalogReview ? ['write_channels'] : []} scopeLevel='system'>
        <ModelsManagement />
      </RouteGuard>
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/models/')({
  component: ProtectedModels,
  validateSearch: (search: Record<string, unknown>): { catalogReview?: z.infer<typeof reviewScope> } => {
    const scope = reviewScope.safeParse(search.catalogReview);
    return scope.success ? { catalogReview: scope.data } : {};
  },
});

defmodule Registrar.World do
  @moduledoc """
  The composition engine — the only code in the registry allowed to
  see across orgs. Composes a device's world as the union of its
  effective grants: per-org self addresses, peers visible through
  shared orgs (carrying only shared-org addresses — the org seam),
  DNS search domains, and per-org filters.

  The return value is the `World(device)` contract the wire head
  consumes. Version it; never leak anything into it that a per-org
  server could not have produced for its own slice.
  """

  alias Registrar.{Grants, Net}

  @version 0

  def for_device(%{id: device_id}), do: for_device(device_id)

  def for_device(device_id) when is_integer(device_id) do
    {mine, others} =
      Enum.split_with(Grants.effective_grants(), &(&1.device_id == device_id))

    my_org_ids = MapSet.new(mine, & &1.org_id)

    peers =
      others
      |> Enum.filter(&MapSet.member?(my_org_ids, &1.org_id))
      |> Enum.group_by(& &1.device_id)
      |> Enum.map(fn {_device_id, [first | _] = shared_grants} ->
        %{
          name: first.device.name,
          node_key: first.device.node_key,
          addresses: Enum.map(shared_grants, &address/1),
          orgs: Enum.map(shared_grants, & &1.org.slug)
        }
      end)
      |> Enum.sort_by(& &1.name)

    %{
      version: @version,
      self: %{
        addresses: Enum.map(mine, &address/1),
        orgs: Enum.map(mine, & &1.org.slug)
      },
      peers: peers,
      dns: %{
        search_domains: Enum.map(mine, &"#{&1.org.slug}.#{base_domain()}")
      },
      filters:
        Enum.map(mine, fn grant ->
          {:ok, prefix} = Net.org_prefix(grant.org.prefix_index)
          %{src: prefix, dst: prefix}
        end)
    }
  end

  defp address(grant) do
    {:ok, addr} = Net.device_address(grant.org.prefix_index, grant.host_index)
    addr
  end

  defp base_domain, do: Application.get_env(:registry, :base_domain, "overlay.internal")
end

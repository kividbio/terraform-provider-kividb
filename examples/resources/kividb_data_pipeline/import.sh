# Import an existing pipeline by its name. The API never returns the source
# password, so set source_password in the configuration: the first apply after
# the import sends it.
terraform import kividb_data_pipeline.shop shop-cdc

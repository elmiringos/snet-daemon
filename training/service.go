//go:generate protoc -I . ./training_daemon.proto ./training.proto --go-grpc_out=paths=source_relative:. --go_out=paths=source_relative:.

package training

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"github.com/singnet/snet-daemon/v5/errs"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	_ "embed"
	"github.com/singnet/snet-daemon/v5/blockchain"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/singnet/snet-daemon/v5/config"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	DateFormat = "02-01-2006"
)

// ModelService this is remote AI service provider
type ModelService struct {
	serviceMetaData      *blockchain.ServiceMetadata
	organizationMetaData *blockchain.OrganizationMetaData
	storage              *ModelStorage
	userStorage          *ModelUserStorage
	serviceUrl           string
}

type DaemonService struct {
	serviceMetaData      *blockchain.ServiceMetadata
	organizationMetaData *blockchain.OrganizationMetaData
	storage              *ModelStorage
	userStorage          *ModelUserStorage
	pendingStorage       *PendingModelStorage
	publicStorage        *PublicModelStorage
	serviceUrl           string
	trainingMetadata     *TrainingMetadata
	methodsMetadata      map[string]*MethodMetadata
}

func (ds *DaemonService) CreateModel(c context.Context, request *NewModelRequest) (*ModelResponse, error) {

	zap.L().Debug("CreateModel request")

	if request == nil || request.Authorization == nil {
		return &ModelResponse{Status: Status_ERRORED}, ErrNoAuthorization
	}

	if err := ds.verifySignature(request.Authorization); err != nil {
		zap.L().Error("unable to create model, bad authorization provided", zap.Error(err))
		return &ModelResponse{Status: Status_ERRORED}, ErrBadAuthorization
	}

	if request.GetModel().GrpcServiceName == "" || request.GetModel().GrpcMethodName == "" {
		zap.L().Error("invalid request, no grpc_method_name or grpc_service_name provided")
		return &ModelResponse{Status: Status_ERRORED}, ErrNoGRPCServiceOrMethod
	}

	request.Model.ServiceId = config.GetString(config.ServiceId)
	request.Model.OrganizationId = config.GetString(config.OrganizationId)
	request.Model.GroupId = ds.organizationMetaData.GetGroupIdString()

	// make a call to the client
	// if the response is successful, store details in etcd
	// send back the response to the client
	conn, client, err := ds.getServiceClient()
	if err != nil {
		zap.L().Error("[CreateModel] unable to getServiceClient", zap.Error(err))
		return &ModelResponse{Status: Status_ERRORED}, WrapError(ErrServiceInvocation, err.Error())
	}

	responseModelID, errClient := client.CreateModel(c, request.Model)
	closeConn(conn)
	if errClient != nil {
		zap.L().Error("[CreateModel] unable to call CreateModel", zap.Error(errClient))
		return &ModelResponse{Status: Status_ERRORED}, WrapError(ErrServiceInvocation, errClient.Error())
	}

	if responseModelID == nil {
		zap.L().Error("[CreateModel] CreateModel returned null response")
		return &ModelResponse{Status: Status_ERRORED}, ErrEmptyResponse
	}

	if responseModelID.ModelId == "" {
		zap.L().Error("[CreateModel] CreateModel returned empty modelID")
		return &ModelResponse{Status: Status_ERRORED}, ErrEmptyModelID
	}

	//store the details in etcd
	zap.L().Debug("Creating model based on response from CreateModel of training service")

	data, err := ds.createModelDetails(request, responseModelID)
	if err != nil {
		zap.L().Error("[CreateModel] Can't save model", zap.Error(err))
		return nil, WrapError(ErrDaemonStorage, err.Error())
	}
	modelResponse := BuildModelResponse(data, Status_CREATED)
	return modelResponse, err
}

func (ds *DaemonService) buildPublicModelKey() *PublicModelKey {
	return &PublicModelKey{
		OrganizationId: config.GetString(config.OrganizationId),
		ServiceId:      config.GetString(config.ServiceId),
		GroupId:        ds.organizationMetaData.GetGroupIdString(),
	}
}

func (ds *DaemonService) getPendingModelIds() (*PendingModelData, error) {
	key := ds.pendingStorage.buildPendingModelKey()

	data, _, err := ds.pendingStorage.Get(key)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (ds *DaemonService) startUpdateModelStatusWorker(ctx context.Context, modelId string) {
	modelKey := ds.storage.buildModelKey(modelId)
	currentModelData, ok, err := ds.storage.Get(modelKey)
	if err != nil {
		zap.L().Error("[startUpdateModelStatusWorker] err in getting modelData from storage", zap.Error(err))
		return
	}
	if !ok {
		zap.L().Error("[startUpdateModelStatusWorker] there is no model with such modelKey", zap.Any("modelKey", modelKey))
		return
	}

	_, client, err := ds.getServiceClient()
	if err != nil {
		zap.L().Error("[startUpdateModelStatusWorker] error in getting service client", zap.Error(err))
		return
	}

	response, err := client.GetModelStatus(ctx, &ModelID{ModelId: modelId})
	if response == nil || err != nil {
		zap.L().Error("[startUpdateModelStatusWorker] error in invoking GetModelStatus, service-provider should implement it", zap.Error(err))
		return
	}

	if response.Status != Status_TRAINING && response.Status != Status_VALIDATING {
		err := ds.pendingStorage.RemovePendingModelId(ds.pendingStorage.buildPendingModelKey(), modelId)
		if err != nil {
			zap.L().Error("[RemovePendingModelId] error in updating model status", zap.Error(err))
		}
	}

	newModelData := *currentModelData // Shallow copy of the current model data.
	// However, it does not create deep copies of any slices contained within ModelData; modifications to the slices in newModelData will affect currentModelData.
	if currentModelData.Status == response.Status {
		// if status don't changed yet we skip it
		return
	}

	newModelData.Status = response.Status
	zap.L().Debug("[startUpdateModelStatusWorker]", zap.String("current status", currentModelData.Status.String()), zap.String("new status", response.Status.String()))
	ok, err = ds.storage.CompareAndSwap(modelKey, currentModelData, &newModelData)
	if !ok || err != nil {
		zap.L().Debug("[startUpdateModelStatusWorker] error in updating model status", zap.Bool("isOK", ok), zap.Error(err))
	}
}

func (ds *DaemonService) updateModelStatusWorker(ctx context.Context, tasks <-chan string, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case modelID := <-tasks:
			go ds.startUpdateModelStatusWorker(ctx, modelID)
		case <-ctx.Done():
			return
		}
	}
}

func (ds *DaemonService) ManageUpdateModelStatusWorkers(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)

	tasks := make(chan string)
	//numWorkers := 1docker
	var wg sync.WaitGroup

	//for i := 0; i < numWorkers; i++ {
	//	wg.Add(1)
	go ds.updateModelStatusWorker(ctx, tasks, &wg)
	//}

	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			data, err := ds.getPendingModelIds()
			if err != nil {
				zap.L().Error("Error in getting pending model IDs", zap.Error(err))
				return
			}
			if data == nil {
				continue
			}
			for _, modelID := range data.ModelIDs {
				tasks <- modelID
			}
		case <-ctx.Done():
			wg.Wait()
			return
		}
	}
}

func (ds *DaemonService) ValidateModelPrice(ctx context.Context, request *AuthValidateRequest) (*PriceInBaseUnit, error) {
	conn, client, err := ds.getServiceClient()
	if client == nil || err != nil {
		return nil, WrapError(ErrServiceIssue, err.Error())
	}
	price, err := client.ValidateModelPrice(ctx, &ValidateRequest{
		ModelId:          request.ModelId,
		TrainingDataLink: request.TrainingDataLink,
	})
	closeConn(conn)
	if err != nil || price == nil {
		zap.L().Error("[ValidateModelPrice] service issue", zap.Error(err))
		return nil, WrapError(ErrServiceIssue, err.Error())
	}
	err = ds.updateModelPrices(request.ModelId, price, nil)
	if err != nil {
		zap.L().Debug("[ValidateModelPrice] can't update model prices")
		return nil, err
	}
	return price, nil
}

func (ds *DaemonService) UploadAndValidate(stream Daemon_UploadAndValidateServer) error {
	var fullData bytes.Buffer
	var modelID string

	zap.L().Debug("start upload and validate")

	conn, client, err := ds.getServiceClient()
	if err != nil {
		zap.L().Debug(err.Error())
		return err
	}
	grpcStream, err := client.UploadAndValidate(context.Background())
	if err != nil {
		zap.L().Error("error in sending UploadAndValidate", zap.Error(err))
		return err
	}

	var stResp *StatusResponse

	zap.L().Debug("UploadAndValidate CALLED")
	for {
		req, err := stream.Recv()
		if req == nil {
			continue
		}
		zap.L().Debug("stream.Recv() for model_id " + req.UploadInput.ModelId)
		if err == io.EOF {
			zap.L().Debug("[UploadAndValidate] EOF")
			stResp, err = grpcStream.CloseAndRecv()
			if err != nil {
				zap.L().Error("[UploadAndValidate] CloseSend error", zap.Error(err))
			}
			break
		}
		if err != nil {
			zap.L().Debug("req is nil?", zap.Bool("bool", req == nil))
			zap.L().Error("error in receiving upload request", zap.Error(err))
			return err
		}

		modelID = req.UploadInput.ModelId

		if modelID == "" {
			return errors.New("modelID can't be empty")
		}

		err = grpcStream.SendMsg(req.UploadInput)
		if err != nil {
			zap.L().Error("error in sending upload validation response", zap.Error(err))
			return err
		}

		zap.L().Debug("Received chunk of data for model", zap.String("modelID", req.UploadInput.ModelId))
		fullData.Write(req.UploadInput.Data)
	}
	zap.L().Debug("Received file for model %s with size %d bytes", zap.String("modelID", modelID), zap.Int("len", fullData.Len()))
	closeConn(conn)
	go func() {
		err := ds.pendingStorage.AddPendingModelId(ds.pendingStorage.buildPendingModelKey(), modelID)
		if err != nil {
			zap.L().Error("[AddPendingModelId]", zap.Error(err))
		}
	}()
	if stResp == nil {
		stResp = &StatusResponse{Status: Status_VALIDATING}
	}
	return stream.SendAndClose(stResp)
}

func (ds *DaemonService) ValidateModel(ctx context.Context, request *AuthValidateRequest) (*StatusResponse, error) {
	conn, client, err := ds.getServiceClient()
	if client == nil || err != nil {
		return &StatusResponse{
			Status: Status_ERRORED,
		}, WrapError(ErrServiceIssue, err.Error())
	}
	statusResp, err := client.ValidateModel(ctx, &ValidateRequest{
		ModelId:          request.ModelId,
		TrainingDataLink: request.TrainingDataLink,
	})
	closeConn(conn)
	if err != nil {
		return nil, WrapError(ErrServiceIssue, err.Error())
	}
	go func() {
		err := ds.pendingStorage.AddPendingModelId(ds.pendingStorage.buildPendingModelKey(), request.ModelId)
		if err != nil {
			zap.L().Error("[AddPendingModelId]", zap.Error(err))
		}
	}()

	return statusResp, nil
}

func (ds *DaemonService) TrainModelPrice(ctx context.Context, request *CommonRequest) (*PriceInBaseUnit, error) {
	conn, client, err := ds.getServiceClient()
	if client == nil || err != nil {
		return nil, WrapError(ErrServiceIssue, err.Error())
	}
	price, err := client.TrainModelPrice(ctx, &ModelID{
		ModelId: request.ModelId,
	})
	closeConn(conn)
	if err != nil {
		zap.L().Debug("[TrainModelPrice] can't update model prices")
		return nil, WrapError(ErrServiceIssue, err.Error())
	}
	err = ds.updateModelPrices(request.ModelId, nil, price)
	if err != nil {
		zap.L().Debug("[TrainModelPrice] can't update model prices")
		return nil, err
	}
	return price, nil
}

func (ds *DaemonService) TrainModel(ctx context.Context, request *CommonRequest) (*StatusResponse, error) {
	conn, client, err := ds.getServiceClient()
	if client == nil || err != nil {
		zap.L().Error("issue with service", zap.Error(err))
		return &StatusResponse{
			Status: Status_ERRORED,
		}, WrapError(ErrServiceIssue, err.Error())
	}
	statusResp, err := client.TrainModel(ctx, &ModelID{
		ModelId: request.ModelId,
	})
	closeConn(conn)
	if err != nil {
		zap.L().Error("[TrainModel] issue with service", zap.Error(err))
		return &StatusResponse{
			Status: Status_ERRORED,
		}, WrapError(ErrServiceIssue, err.Error())
	}
	go func() {
		err := ds.pendingStorage.AddPendingModelId(ds.pendingStorage.buildPendingModelKey(), request.ModelId)
		if err != nil {
			zap.L().Error("[AddPendingModelId]", zap.Error(err))
		}
	}()
	return statusResp, nil
}

func (ds *DaemonService) GetTrainingMetadata(ctx context.Context, empty *emptypb.Empty) (*TrainingMetadata, error) {
	return ds.trainingMetadata, nil
}

func (ds *DaemonService) UpdateModel(ctx context.Context, request *UpdateModelRequest) (*ModelResponse, error) {

	if request == nil || request.Authorization == nil {
		return &ModelResponse{Status: Status_ERRORED}, ErrNoAuthorization
	}
	if err := ds.verifySignature(request.Authorization); err != nil {
		return &ModelResponse{Status: Status_ERRORED},
			WrapError(ErrAccessToModel, err.Error())
	}
	if err := ds.verifySignerHasAccessToTheModel(request.ModelId, request.Authorization.SignerAddress); err != nil {
		return &ModelResponse{},
			WrapError(ErrAccessToModel, err.Error())
	}
	if request.ModelId == "" {
		return &ModelResponse{Status: Status_ERRORED}, ErrEmptyModelID
	}
	if err := ds.verifyCreatedByAddress(request.ModelId, request.Authorization.SignerAddress); err != nil {
		return &ModelResponse{}, err
	}

	zap.L().Info("Updating model")
	data, err := ds.updateModelDetails(request)
	if err != nil || data == nil {
		return &ModelResponse{Status: Status_ERRORED},
			WrapError(ErrDaemonStorage, err.Error())
	}
	return BuildModelResponse(data, data.Status), nil
}

func (ds *DaemonService) GetMethodMetadata(ctx context.Context, request *MethodMetadataRequest) (*MethodMetadata, error) {
	if request.GetModelId() != "" {
		data, err := ds.storage.GetModel(request.ModelId)
		if err != nil || data == nil {
			zap.L().Error("[GetMethodMetadata] can't get model data", zap.Error(err))
			return nil, WrapError(ErrGetModelStorage, err.Error())
		}
		request.GrpcMethodName = data.GRPCMethodName
		request.GrpcServiceName = data.GRPCServiceName
	}
	key := request.GrpcServiceName + request.GrpcMethodName
	return ds.methodsMetadata[key], nil
}

func closeConn(conn *grpc.ClientConn) {
	if conn == nil {
		zap.L().Debug("conn is nil!")
		return
	}
	err := conn.Close()
	if err != nil {
		zap.L().Error("error in closing Client Connection", zap.Error(err))
	}
}

// deprecated
//func deferConnection(conn *grpc.ClientConn) {
//	if conn == nil {
//		zap.L().Debug("conn is nil!")
//		return
//	}
//	defer func(conn *grpc.ClientConn) {
//		err := conn.Close()
//		if err != nil {
//			zap.L().Error("error in closing Client Connection", zap.Error(err))
//		}
//	}(conn)
//}

func getConnection(endpoint string) (conn *grpc.ClientConn) {
	options := grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(config.GetInt(config.MaxMessageSizeInMB)*1024*1024),
		grpc.MaxCallSendMsgSize(config.GetInt(config.MaxMessageSizeInMB)*1024*1024))

	passthroughURL, err := url.Parse(endpoint)
	if err != nil {
		zap.L().Panic("error parsing passthrough endpoint", zap.Error(err))
	}
	if strings.Compare(passthroughURL.Scheme, "https") == 0 {
		conn, err = grpc.NewClient(passthroughURL.Host,
			grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")), options)
		if err != nil {
			zap.L().Panic("error dialing service", zap.Error(err))
		}
	} else {
		conn, err = grpc.NewClient(passthroughURL.Host, grpc.WithTransportCredentials(insecure.NewCredentials()), options)
		if err != nil {
			zap.L().Panic("error dialing service", zap.Error(err))
		}
	}
	return
}

func (ds *DaemonService) getServiceClient() (conn *grpc.ClientConn, client ModelClient, err error) {
	conn = getConnection(ds.serviceUrl)
	client = NewModelClient(conn)
	return
}

func (ds *DaemonService) createModelDetails(request *NewModelRequest, response *ModelID) (*ModelData, error) {
	key := ds.storage.buildModelKey(response.ModelId)
	data := ds.getModelDataToCreate(request, response)

	//store the model details in etcd
	zap.L().Debug("createModelDetails", zap.Any("key", key))
	err := ds.storage.Put(key, data)
	if err != nil {
		zap.L().Error("can't put model in etcd", zap.Error(err))
		return nil, err
	}

	// for every accessible address in the list, store the user address and all the model Ids associated with it
	for _, address := range data.AuthorizedAddresses {
		userKey := getModelUserKey(key, address)
		userData := ds.getModelUserData(key, address)
		zap.L().Debug("createModelDetails", zap.Any("userKey", userKey))
		err = ds.userStorage.Put(userKey, userData)
		if err != nil {
			zap.L().Error("can't put in user storage", zap.Error(err))
			return nil, err
		}
		zap.L().Debug("creating training model", zap.String("userKey", userKey.String()), zap.String("userData", userData.String()))
	}

	if request.Model.IsPublic {
		publicModelKey := ds.buildPublicModelKey()
		err := ds.publicStorage.AddPublicModelId(publicModelKey, response.ModelId)
		if err != nil {
			zap.L().Error("error in adding model id to public model storage")
		}
		zap.L().Debug("adding model id to public model storage", zap.String("modelId", response.ModelId), zap.String("key", publicModelKey.String()))
	}

	return data, nil
}

func getModelUserKey(key *ModelKey, address string) *ModelUserKey {
	return &ModelUserKey{
		OrganizationId: key.OrganizationId,
		ServiceId:      key.ServiceId,
		GroupId:        key.GroupId,
		UserAddress:    address,
	}
}

func (ds *DaemonService) getModelUserData(key *ModelKey, address string) *ModelUserData {
	//Check if there are any model Ids already associated with this user
	modelIds := make([]string, 0)
	userKey := getModelUserKey(key, address)
	zap.L().Debug("user model key is" + userKey.String())
	data, ok, err := ds.userStorage.Get(userKey)
	if ok && err == nil && data != nil {
		modelIds = data.ModelIds
	}
	if err != nil {
		zap.L().Error("[getModelUserData] can't get model data from etcd", zap.Error(err))
	}
	modelIds = append(modelIds, key.ModelId)
	return &ModelUserData{
		OrganizationId: key.OrganizationId,
		ServiceId:      key.ServiceId,
		GroupId:        key.GroupId,
		UserAddress:    address,
		ModelIds:       modelIds,
	}
}

func (ds *DaemonService) deleteUserModelDetails(key *ModelKey, data *ModelData) (err error) {
	for _, address := range data.AuthorizedAddresses {
		userKey := getModelUserKey(key, address)
		dataStorage, ok, err := ds.userStorage.Get(userKey)
		if !ok || err != nil || dataStorage == nil {
			zap.L().Error("[deleteUserModelDetails] can't get user data", zap.Error(err))
			continue
		}
		dataStorage.ModelIds = remove(dataStorage.ModelIds, key.ModelId)
		err = ds.userStorage.Put(userKey, dataStorage)
		if err != nil {
			zap.L().Error("can't remove access to model", zap.Error(err), zap.String("userKey", userKey.String()), zap.String("modelID", key.ModelId))
		}
	}
	return
}

func (ds *DaemonService) deleteModelDetails(req *CommonRequest) (data *ModelData, err error) {
	key := ds.storage.buildModelKey(req.ModelId)
	ok := false
	data, ok, err = ds.storage.Get(key)
	if data == nil || !ok || err != nil {
		zap.L().Debug("Can't find model to delete", zap.String("key", key.String()))
		return nil, errors.New("can't find model to delete")
	}
	data.Status = Status_DELETED
	data.UpdatedDate = fmt.Sprintf("%v", time.Now().Format(DateFormat))
	err = ds.storage.Put(key, data)
	if err != nil {
		zap.L().Error("can't update status to DELETED", zap.Error(err))
	}
	err = ds.deleteUserModelDetails(key, data)
	return
}

func convertModelDataToBO(data *ModelData) (responseData *ModelResponse) {
	responseData = &ModelResponse{
		ModelId:          data.ModelId,
		GrpcMethodName:   data.GRPCMethodName,
		GrpcServiceName:  data.GRPCServiceName,
		Description:      data.Description,
		IsPublic:         data.IsPublic,
		AddressList:      data.AuthorizedAddresses,
		TrainingDataLink: data.TrainingLink,
		Name:             data.ModelName,
		//OrganizationId:       data.OrganizationId,
		//ServiceId:            data.ServiceId,
		//GroupId:              data.GroupId,
		UpdatedDate: data.UpdatedDate,
		Status:      data.Status,
	}
	return
}

func (ds *DaemonService) updateModelDetails(request *UpdateModelRequest) (data *ModelData, err error) {
	key := ds.storage.buildModelKey(request.ModelId)
	oldAddresses := make([]string, 0)
	var latestAddresses []string
	// by default add the creator to the Authorized list of Address
	if request.AddressList != nil || len(request.AddressList) > 0 {
		latestAddresses = request.AddressList
	}
	latestAddresses = append(latestAddresses, request.Authorization.SignerAddress) // add creator
	if data, err = ds.storage.GetModel(request.ModelId); err == nil && data != nil {
		oldAddresses = data.AuthorizedAddresses
		data.AuthorizedAddresses = latestAddresses
		latestAddresses = append(latestAddresses, request.Authorization.SignerAddress)
		data.UpdatedByAddress = request.Authorization.SignerAddress
		data.ModelName = request.ModelName
		data.UpdatedDate = fmt.Sprintf("%v", time.Now().Format(DateFormat))
		data.Description = request.Description

		err = ds.storage.Put(key, data)
		if err != nil {
			zap.L().Error("Error in putting data in user storage", zap.Error(err))
		}
		//get the difference of all the addresses b/w old and new
		updatedAddresses := difference(oldAddresses, latestAddresses)
		for _, address := range updatedAddresses {
			modelUserKey := getModelUserKey(key, address)
			modelUserData := ds.getModelUserData(key, address)
			//if the address is present in the request but not in the old address , add it to the storage
			if isValuePresent(address, request.AddressList) {
				modelUserData.ModelIds = append(modelUserData.ModelIds, request.ModelId)
			} else { // the address was present in the old data , but not in new , hence needs to be deleted
				modelUserData.ModelIds = remove(modelUserData.ModelIds, request.ModelId)
			}
			err = ds.userStorage.Put(modelUserKey, modelUserData)
			if err != nil {
				zap.L().Error("Error in putting data in storage", zap.Error(err))
			}
		}
	}
	return
}

// ensure only authorized use
func (ds *DaemonService) verifySignerHasAccessToTheModel(modelId string, address string) (err error) {
	// check if model is public
	publicModelKey := ds.buildPublicModelKey()
	publicModels, ok, err := ds.publicStorage.Get(publicModelKey)
	if ok && err == nil {
		if slices.Contains(publicModels.ModelIDs, modelId) {
			return
		}
	}

	// check user access if model is not public
	userModelKey := &ModelUserKey{
		OrganizationId: config.GetString(config.OrganizationId),
		ServiceId:      config.GetString(config.ServiceId),
		GroupId:        ds.organizationMetaData.GetGroupIdString(),
		UserAddress:    address,
	}
	userModelsData, ok, err := ds.userStorage.Get(userModelKey)

	if err != nil {
		return WrapError(ErrGetUserModelStorage, err.Error())
	}

	if !ok {
		return fmt.Errorf("user %v, does not have access to model Id %v", address, modelId)
	}

	if !slices.Contains(userModelsData.ModelIds, modelId) {
		return fmt.Errorf("user %v, does not have access to model Id %v", address, modelId)
	}

	return
}

// ensure only owner can update the model state
func (ds *DaemonService) verifyCreatedByAddress(modelId, address string) (err error) {
	modelKey := &ModelKey{
		OrganizationId: config.GetString(config.OrganizationId),
		ServiceId:      config.GetString(config.ServiceId),
		GroupId:        ds.organizationMetaData.GetGroupIdString(),
		ModelId:        modelId,
	}

	modelData, ok, err := ds.storage.Get(modelKey)
	if err != nil {
		return WrapError(ErrGetModelStorage, err.Error())
	}

	if !ok {
		return WrapError(ErrGetModelStorage, fmt.Sprintf("model data doesn't for key: %s", modelKey))
	}

	if modelData.CreatedByAddress != address {
		return ErrNotOwnerModel
	}

	return
}

func (ds *DaemonService) updateModelStatus(modelID string, newStatus Status) (data *ModelData, err error) {
	key := &ModelKey{
		OrganizationId: config.GetString(config.OrganizationId),
		ServiceId:      config.GetString(config.ServiceId),
		GroupId:        ds.organizationMetaData.GetGroupIdString(),
		ModelId:        modelID,
	}
	ok := false
	zap.L().Debug("[updateModelStatus]", zap.String("modelID", key.ModelId))
	data, ok, err = ds.storage.Get(key)
	if err != nil || !ok || data == nil {
		zap.L().Error("[updateModelStatus] can't get model data from etcd", zap.Error(err))
		return data, WrapError(ErrGetModelStorage, err.Error())
	}
	data.Status = newStatus
	if err = ds.storage.Put(key, data); err != nil {
		zap.L().Error("[updateModelStatus] issue with retrieving model data from storage", zap.Error(err))
		return data, WrapError(ErrGetModelStorage, err.Error())
	}
	return
}

func (ds *DaemonService) updateModelPrices(modelID string, validatePrice, trainPrice *PriceInBaseUnit) error {
	key := &ModelKey{
		OrganizationId: config.GetString(config.OrganizationId),
		ServiceId:      config.GetString(config.ServiceId),
		GroupId:        ds.organizationMetaData.GetGroupIdString(),
		ModelId:        modelID,
	}
	zap.L().Debug("[updateModelPrices]", zap.String("modelID", key.ModelId))
	data, ok, err := ds.storage.Get(key)
	if err != nil || !ok || data == nil {
		zap.L().Error("[updateModelPrices] can't get model data from etcd", zap.Error(err))
		return errors.New("can't get model data from etcd")
	}
	if validatePrice != nil {
		data.ValidatePrice = validatePrice.Price
	}
	if trainPrice != nil {
		data.TrainPrice = trainPrice.Price
	}
	if err = ds.storage.Put(key, data); err != nil {
		zap.L().Error("[updateModelPrices] issue with updating model data", zap.Error(err))
		return fmt.Errorf("[updateModelPrices] issue with updating model data: %s", err)
	}
	return err
}

func (ds *DaemonService) GetAllModels(c context.Context, request *AllModelsRequest) (*ModelsResponse, error) {
	if request == nil || request.Authorization == nil {
		return &ModelsResponse{}, ErrNoAuthorization
	}
	if err := ds.verifySignature(request.Authorization); err != nil {
		return &ModelsResponse{}, ErrBadAuthorization
	}

	zap.L().Debug("[GetAllModels]", zap.Any("request", request))

	if request.Statuses == nil || len(request.Statuses) == 0 {
		request.Statuses = []Status{Status_CREATED, Status_VALIDATING, Status_VALIDATED, Status_TRAINING, Status_READY_TO_USE, Status_ERRORED, Status_DELETED}
	}

	modelDetailsArray := make([]*ModelResponse, 0)

	if request.IsPublic == nil || !*request.IsPublic {

		userModelKey := &ModelUserKey{
			OrganizationId: config.GetString(config.OrganizationId),
			ServiceId:      config.GetString(config.ServiceId),
			GroupId:        ds.organizationMetaData.GetGroupIdString(),
			UserAddress:    request.Authorization.SignerAddress,
		}

		if data, ok, err := ds.userStorage.Get(userModelKey); data != nil && ok && err == nil {
			modelKey := &ModelKey{
				OrganizationId: config.GetString(config.OrganizationId),
				ServiceId:      config.GetString(config.ServiceId),
				GroupId:        ds.organizationMetaData.GetGroupIdString(),
			}
			for _, modelId := range data.ModelIds {
				modelKey.ModelId = modelId
				if modelData, modelOk, modelErr := ds.storage.Get(modelKey); modelOk && modelData != nil && modelErr == nil {
					boModel := convertModelDataToBO(modelData)
					modelDetailsArray = append(modelDetailsArray, boModel)
				}
			}
		}
	}

	if request.IsPublic == nil || *request.IsPublic {

		publicModelKey := &PublicModelKey{
			OrganizationId: config.GetString(config.OrganizationId),
			ServiceId:      config.GetString(config.ServiceId),
			GroupId:        ds.organizationMetaData.GetGroupIdString(),
		}

		if data, ok, err := ds.publicStorage.Get(publicModelKey); data != nil && ok && err == nil {
			modelKey := &ModelKey{
				OrganizationId: config.GetString(config.OrganizationId),
				ServiceId:      config.GetString(config.ServiceId),
				GroupId:        ds.organizationMetaData.GetGroupIdString(),
			}
			for _, modelId := range data.ModelIDs {
				modelKey.ModelId = modelId
				if modelData, modelOk, modelErr := ds.storage.Get(modelKey); modelOk && modelData != nil && modelErr == nil {
					boModel := convertModelDataToBO(modelData)
					modelDetailsArray = append(modelDetailsArray, boModel)
				}
			}
		}
	}

	filtered := modelDetailsArray[:0]

	for _, v := range modelDetailsArray {
		zap.L().Debug("[GetAllModels] model", zap.String("id", v.ModelId), zap.String("status", v.Status.String()))
		if strings.Contains(v.Name, request.Name) &&
			strings.Contains(v.GrpcMethodName, request.GrpcMethodName) &&
			strings.Contains(v.GrpcServiceName, request.GrpcServiceName) &&
			strings.Contains(v.CreatedByAddress, request.CreatedByAddress) &&
			slices.Contains(request.Statuses, v.Status) {
			filtered = append(filtered, v)
		}
	}

	if request.Page != 0 || request.PageSize != 0 {
		filtered = filtered[request.Page*request.PageSize : request.PageSize]
	}

	return &ModelsResponse{ListOfModels: filtered}, nil
}

func (ds *DaemonService) getModelDataToCreate(request *NewModelRequest, response *ModelID) (data *ModelData) {
	data = &ModelData{
		Status:              Status_CREATED,
		GRPCServiceName:     request.Model.GrpcServiceName,
		GRPCMethodName:      request.Model.GrpcMethodName,
		CreatedByAddress:    request.Authorization.SignerAddress,
		UpdatedByAddress:    request.Authorization.SignerAddress,
		AuthorizedAddresses: request.Model.AddressList,
		Description:         request.Model.Description,
		ModelName:           request.Model.Name,
		IsPublic:            request.Model.IsPublic,
		IsDefault:           false,
		ModelId:             response.ModelId,
		OrganizationId:      config.GetString(config.OrganizationId),
		ServiceId:           config.GetString(config.ServiceId),
		GroupId:             ds.organizationMetaData.GetGroupIdString(),
		UpdatedDate:         fmt.Sprintf("%v", time.Now().Format(DateFormat)),
	}
	//by default add the creator to the Authorized list of Address
	if data.AuthorizedAddresses == nil {
		data.AuthorizedAddresses = make([]string, 0)
	}
	data.AuthorizedAddresses = append(data.AuthorizedAddresses, data.CreatedByAddress)
	return
}

func BuildModelResponse(data *ModelData, status Status) *ModelResponse {
	return &ModelResponse{
		Status:           status,
		ModelId:          data.ModelId,
		GrpcMethodName:   data.GRPCMethodName,
		GrpcServiceName:  data.GRPCServiceName,
		Description:      data.Description,
		IsPublic:         data.IsPublic,
		AddressList:      data.AuthorizedAddresses,
		TrainingDataLink: data.TrainingLink,
		Name:             data.ModelName,
		UpdatedDate:      data.UpdatedDate,
		CreatedDate:      data.CreatedDate,
		CreatedByAddress: data.CreatedByAddress,
	}
}

func (ds *DaemonService) DeleteModel(c context.Context, req *CommonRequest) (*StatusResponse, error) {

	if req == nil || req.Authorization == nil {
		return &StatusResponse{Status: Status_ERRORED}, ErrNoAuthorization
	}

	if req.ModelId == "" {
		return &StatusResponse{Status: Status_ERRORED}, ErrEmptyModelID
	}

	if err := ds.verifySignature(req.Authorization); err != nil {
		return &StatusResponse{Status: Status_ERRORED},
			WrapError(ErrAccessToModel, err.Error())
	}

	if err := ds.verifySignerHasAccessToTheModel(req.ModelId, req.Authorization.SignerAddress); err != nil {
		return &StatusResponse{},
			WrapError(ErrAccessToModel, err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
	defer cancel()
	conn, client, err := ds.getServiceClient()
	if err != nil {
		return &StatusResponse{Status: Status_ERRORED},
			WrapError(ErrServiceInvocation, err.Error())
	}
	response, errModel := client.DeleteModel(ctx, &ModelID{ModelId: req.ModelId})
	closeConn(conn)
	if response == nil || errModel != nil {
		zap.L().Error("error in invoking DeleteModel, service-provider should realize it", zap.Error(errModel))
		return &StatusResponse{Status: Status_ERRORED}, fmt.Errorf("error in invoking DeleteModel, service-provider should realize it")
	}
	data, err := ds.deleteModelDetails(req)
	if err != nil || data == nil {
		zap.L().Error("issue with deleting ModelId in storage", zap.Error(err))
		return response, WrapError(ErrDaemonStorage, fmt.Sprintf("issue with deleting Model %v", err))
	}
	return &StatusResponse{Status: Status_DELETED}, err
}

func (ds *DaemonService) GetModel(c context.Context, request *CommonRequest) (response *ModelResponse, err error) {
	if request == nil || request.Authorization == nil {
		return &ModelResponse{Status: Status_ERRORED}, ErrNoAuthorization
	}
	if err = ds.verifySignature(request.Authorization); err != nil {
		return &ModelResponse{Status: Status_ERRORED}, ErrBadAuthorization
	}
	if request.ModelId == "" {
		return &ModelResponse{Status: Status_ERRORED}, ErrEmptyModelID
	}
	if err = ds.verifySignerHasAccessToTheModel(request.ModelId, request.Authorization.SignerAddress); err != nil {
		return &ModelResponse{},
			WrapError(ErrAccessToModel, err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*20)
	defer cancel()

	if conn, client, err := ds.getServiceClient(); err == nil {
		responseStatus, err := client.GetModelStatus(ctx, &ModelID{ModelId: request.ModelId})
		if responseStatus == nil || err != nil {
			zap.L().Error("error in invoking GetModelStatus, service-provider should realize it", zap.Error(err))
			return &ModelResponse{Status: Status_ERRORED}, fmt.Errorf("error in invoking GetModelStatus, service-provider should realize it")
		}
		zap.L().Info("[GetModelStatus] response from service-provider", zap.Any("response", responseStatus))
		zap.L().Debug("[GetModelStatus] updating model status based on response from UpdateModel")
		data, err := ds.updateModelStatus(request.ModelId, responseStatus.Status)
		closeConn(conn)
		zap.L().Debug("[GetModelStatus] data that be returned to client", zap.Any("data", data))
		if err == nil && data != nil {
			response = BuildModelResponse(data, responseStatus.Status)
		} else {
			zap.L().Error("[GetModelStatus] BuildModelResponse error", zap.Error(err))
			return response, fmt.Errorf("issue with storage %v", err)
		}
	} else {
		return &ModelResponse{Status: Status_ERRORED}, fmt.Errorf("[GetModelStatus] error in invoking service for Model Training")
	}
	return
}

// getFileDescriptors converts text of proto files to bufbuild linker
func getFileDescriptorsWithTraining(protoFiles map[string]string) linker.Files {
	protoFiles["training.proto"] = TrainingProtoEmbeded
	accessor := protocompile.SourceAccessorFromMap(protoFiles)
	r := protocompile.WithStandardImports(&protocompile.SourceResolver{Accessor: accessor})
	compiler := protocompile.Compiler{
		Resolver:       r,
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	fds, err := compiler.Compile(context.Background(), slices.Collect(maps.Keys(protoFiles))...)
	if err != nil || fds == nil {
		zap.L().Fatal("failed to analyze protofile"+errs.ErrDescURL(errs.InvalidProto), zap.Error(err))
	}
	return fds
}

// NewTrainingService daemon self server
func NewTrainingService(serMetaData *blockchain.ServiceMetadata,
	orgMetadata *blockchain.OrganizationMetaData, storage *ModelStorage, userStorage *ModelUserStorage,
	pendingStorage *PendingModelStorage, publicStorage *PublicModelStorage) DaemonServer {

	serMetaData.ProtoDescriptors = getFileDescriptorsWithTraining(serMetaData.ProtoFiles)

	methodsMD, trainMD, err := parseTrainingMetadata(serMetaData.ProtoDescriptors)
	if err != nil {
		zap.L().Error("[NewTrainingService] can't init training", zap.Error(err))
		return &NoTrainingDaemonServer{}
	}

	serviceURL := config.GetString(config.ModelMaintenanceEndPoint)
	if config.IsValidUrl(serviceURL) && config.GetBool(config.BlockchainEnabledKey) {

		daemonService := &DaemonService{
			serviceMetaData:      serMetaData,
			organizationMetaData: orgMetadata,
			storage:              storage,
			userStorage:          userStorage,
			pendingStorage:       pendingStorage,
			publicStorage:        publicStorage,
			serviceUrl:           serviceURL,
			trainingMetadata:     trainMD,
			methodsMetadata:      methodsMD,
		}

		go daemonService.ManageUpdateModelStatusWorkers(context.Background(), 3*time.Second)

		return daemonService
	}

	return &NoTrainingDaemonServer{}
}
